package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

// UpstreamProxy represents ONE forward proxy (CONNECT) VPS for a set. Requests are
// tunneled through it to the set's real origin with end-to-end TLS — the proxy only
// pipes bytes, so there is a single TLS handshake (bestproxy↔origin) that we reuse.
type UpstreamProxy struct {
	Addr    string // forward proxy host:port (for dashboard/health)
	SetName string
	Stats   *stats.ProxyStats

	status     atomic.Uint32
	origin     *url.URL // real upstream, e.g. https://openrouter.ai
	forwardURL *url.URL // forward proxy URL with scheme + basic-auth userinfo
	rt         *trackingTransport
	warmClient *http.Client
}

func NewUpstream(setName string, forwardURL, origin *url.URL, pool config.PoolConfig, tlsInsecure bool) *UpstreamProxy {
	u := &UpstreamProxy{
		Addr:       forwardURL.Host,
		SetName:    setName,
		Stats:      stats.New(),
		origin:     origin,
		forwardURL: forwardURL,
	}

	ps := u.Stats.Pool
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		// Proxy returns the forward proxy → Go opens CONNECT and does TLS to the
		// real origin INSIDE the tunnel (end-to-end). Userinfo in the URL becomes
		// Proxy-Authorization on the CONNECT.
		Proxy: http.ProxyURL(forwardURL),
		// DialContext here dials the PROXY (not the origin), so trackedConn counts
		// tunnel TCP connections — exactly the pooled-keepalive metric we want.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			ps.ConnCreated()
			return &trackedConn{Conn: conn, pool: ps}, nil
		},
		// Shared for TLS-to-proxy (https proxy) and TLS-to-origin (inside tunnel);
		// Go sets ServerName per hop, so the origin cert is verified against the real
		// host — the end-to-end security guarantee. insecure_skip_verify is e2e-only.
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: tlsInsecure, NextProtos: []string{"http/1.1"}}, //nolint:gosec
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   pool.Max,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Force HTTP/1.1 to origin inside the CONNECT tunnel: one TCP conn per in-flight
		// request avoids HTTP/2 head-of-line blocking and the shared connection-level flow
		// window over the geo hop (same reasoning as the reverse-mode h1 switch). There is
		// no per-request failover, so a stale pooled conn surfacing as a RoundTrip error on
		// reuse returns 502 (POST bodies are streamed, not replayable); the health checker
		// keeps dead upstreams out of selection.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}

	u.rt = &trackingTransport{Transport: transport, pool: ps}
	u.warmClient = &http.Client{Transport: transport, Timeout: 15 * time.Second}

	return u
}

// RoundTrip executes one request through this upstream's warm, in-flight-tracked transport.
func (u *UpstreamProxy) RoundTrip(req *http.Request) (*http.Response, error) {
	return u.rt.RoundTrip(req)
}

// Origin is the real upstream URL this forward proxy tunnels to.
func (u *UpstreamProxy) Origin() *url.URL { return u.origin }

// EWMA exposes the selection metric (probe latency EWMA).
func (u *UpstreamProxy) EWMA() float64 { return u.Stats.EWMA() }

// Record* delegate to Stats so *UpstreamProxy satisfies the upstream interface.
func (u *UpstreamProxy) RecordRequest()        { u.Stats.RecordRequest() }
func (u *UpstreamProxy) RecordSuccess(n int64) { u.Stats.RecordSuccess(n) }
func (u *UpstreamProxy) RecordError()          { u.Stats.RecordError() }

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
	pool *stats.PoolStats
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(c.pool.ConnClosed)
	return c.Conn.Close()
}

func (u *UpstreamProxy) Status() Status {
	return Status(u.status.Load())
}

// WarmUp pre-fills the idle pool with n parallel HEAD requests to the origin THROUGH
// the tunnel, establishing reusable end-to-end TLS sessions. Errors/4xx are ignored —
// the goal is only the pooled TLS session (body must be drained for reuse).
func (u *UpstreamProxy) WarmUp(ctx context.Context, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.origin.String()+"/", nil)
			if err != nil {
				return
			}
			resp, err := u.warmClient.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body) //nolint:errcheck
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// Probe checks whether this forward proxy is usable and records latency into EWMA.
//   - mode "tcp":     plain TCP dial to the proxy (cheapest, validates only the socket).
//   - mode "connect": full tunnel setup (dial → TLS-to-proxy → CONNECT origin → 200),
//     validating reachability + proxy TLS + basic auth + tunnel, latency = setup time.
//     Stops at CONNECT — never issues a request to the real origin (no API traffic/WAF).
func (u *UpstreamProxy) Probe(ctx context.Context, mode string) (time.Duration, error) {
	if mode == "tcp" {
		start := time.Now()
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Addr)
		latency := time.Since(start)
		if err != nil {
			return 0, err
		}
		conn.Close()
		return latency, nil
	}
	return u.connectProbe(ctx)
}

func (u *UpstreamProxy) connectProbe(ctx context.Context) (time.Duration, error) {
	start := time.Now()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.forwardURL.Host)
	if err != nil {
		return 0, fmt.Errorf("dial proxy: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl) //nolint:errcheck
	}

	// TLS to the forward proxy itself (when it is an https forward proxy).
	if u.forwardURL.Scheme == "https" {
		host := u.forwardURL.Hostname()
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: u.warmTLSInsecure(),
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, fmt.Errorf("tls to proxy: %w", err)
		}
		conn = tlsConn
	}

	// CONNECT origin:port through the proxy.
	target := net.JoinHostPort(u.origin.Hostname(), originPort(u.origin))
	var authLine string
	if ui := u.forwardURL.User; ui != nil {
		pass, _ := ui.Password()
		token := base64.StdEncoding.EncodeToString([]byte(ui.Username() + ":" + pass))
		authLine = "Proxy-Authorization: Basic " + token + "\r\n"
	}
	reqLine := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, authLine)
	if _, err := io.WriteString(conn, reqLine); err != nil {
		return 0, fmt.Errorf("write CONNECT: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, fmt.Errorf("read CONNECT response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("CONNECT failed: %s", resp.Status)
	}
	return time.Since(start), nil
}

// warmTLSInsecure mirrors the transport's InsecureSkipVerify so the probe trusts the
// same certs as real traffic (both share the e2e-test escape hatch).
func (u *UpstreamProxy) warmTLSInsecure() bool {
	if t, ok := u.warmClient.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
		return t.TLSClientConfig.InsecureSkipVerify
	}
	return false
}

func originPort(o *url.URL) string {
	if p := o.Port(); p != "" {
		return p
	}
	return "443"
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
