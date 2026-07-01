package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elkin/bestproxy/internal/config"
	"github.com/elkin/bestproxy/internal/stats"
)

type Status uint32

const (
	StatusUp   Status = iota
	StatusDown Status = iota
)

type UpstreamProxy struct {
	Addr    string
	SetName string
	// Backup marks this upstream as a reserve, used only when every primary
	// upstream in the set is down.
	Backup bool
	Stats  *stats.ProxyStats

	status     atomic.Uint32
	rp         *httputil.ReverseProxy
	target     *url.URL
	warmClient *http.Client
}

func NewUpstream(setName, addr string, backup bool, pool config.PoolConfig) *UpstreamProxy {
	target := &url.URL{Scheme: "https", Host: addr}

	u := &UpstreamProxy{
		Addr:    addr,
		SetName: setName,
		Backup:  backup,
		Stats:   stats.New(),
		target:  target,
	}

	ps := u.Stats.Pool
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		// Wrap DialContext to track connection creation and closure.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			ps.ConnCreated()
			return &trackedConn{Conn: conn, pool: ps}, nil
		},
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   pool.Max,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Force HTTP/1.1 to OpenRouter: with HTTP/2 all proxied traffic is
		// multiplexed onto a single TLS connection per upstream, which under
		// concurrency suffers TCP head-of-line blocking, a shared flow-control
		// window and a serialized frame writer — the direct client, opening one
		// connection per request, avoids all of that. A large keep-alive pool
		// (MaxIdleConnsPerHost) gives each in-flight request its own TCP window
		// with amortized TLS handshakes.
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{NextProtos: []string{"http/1.1"}},
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}

	u.warmClient = &http.Client{Transport: transport, Timeout: 10 * time.Second}

	// trackingTransport wraps transport to count in-flight requests.
	rt := &trackingTransport{Transport: transport, pool: ps}

	setPrefix := "/" + setName

	u.rp = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = addr
			if strings.HasPrefix(req.URL.Path, setPrefix) {
				req.URL.Path = req.URL.Path[len(setPrefix):]
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}
			}
			if strings.HasPrefix(req.URL.RawPath, setPrefix) {
				req.URL.RawPath = req.URL.RawPath[len(setPrefix):]
			}
			req.Host = addr
			req.Header.Del("X-Forwarded-For")
		},
		Transport: rt,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			u.Stats.RecordError()
			http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			u.Stats.RecordSuccess(resp.ContentLength)
			return nil
		},
	}

	return u
}

// trackingTransport counts in-flight requests around the real transport.
type trackingTransport struct {
	Transport *http.Transport
	pool      *stats.PoolStats
}

func (t *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.pool.ReqStart()
	resp, err := t.Transport.RoundTrip(req)
	t.pool.ReqDone()
	return resp, err
}

// trackedConn wraps net.Conn to decrement pool count on close.
type trackedConn struct {
	net.Conn
	pool  *stats.PoolStats
	once  sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(c.pool.ConnClosed)
	return c.Conn.Close()
}

func (u *UpstreamProxy) Status() Status {
	return Status(u.status.Load())
}

func (u *UpstreamProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.Stats.RecordRequest()
	u.rp.ServeHTTP(w, r)
}

// WarmUp pre-fills the transport's idle pool with n parallel HEAD requests.
// Errors are ignored — the goal is only to establish TLS sessions.
func (u *UpstreamProxy) WarmUp(ctx context.Context, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+u.Addr+"/", nil)
			if err != nil {
				return
			}
			resp, err := u.warmClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

func (u *UpstreamProxy) UpdateStatus(failThreshold, recoverThreshold int) {
	consecFails := int(u.Stats.ConsecFails)
	consecOK := int(u.Stats.ConsecOK)

	switch u.Status() {
	case StatusUp:
		if consecFails >= failThreshold {
			u.status.Store(uint32(StatusDown))
		}
	case StatusDown:
		if consecOK >= recoverThreshold {
			u.status.Store(uint32(StatusUp))
		}
	}
}
