package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/elkin/bestproxy/internal/config"
)

// startFakeConnectProxy starts a plain-TCP CONNECT proxy. If wantAuth != "" it requires
// that exact Proxy-Authorization header value, else returns 407.
func startFakeConnectProxy(t *testing.T, wantAuth string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				if req.Method != http.MethodConnect {
					io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
					return
				}
				if wantAuth != "" && req.Header.Get("Proxy-Authorization") != wantAuth {
					io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					return
				}
				io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func mustUpstream(t *testing.T, proxyAddr, user, pass string) *UpstreamProxy {
	t.Helper()
	fwd := &url.URL{Scheme: "http", Host: proxyAddr}
	if user != "" {
		fwd.User = url.UserPassword(user, pass)
	}
	origin, _ := url.Parse("https://origin.test:443")
	return NewUpstream("test", fwd, origin, false, config.PoolConfig{Max: 10}, true)
}

func TestConnectProbe_Success(t *testing.T) {
	addr := startFakeConnectProxy(t, basicAuth("u", "p"))
	u := mustUpstream(t, addr, "u", "p")

	lat, err := u.Probe(context.Background(), "connect")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if lat <= 0 {
		t.Fatalf("want positive latency, got %v", lat)
	}
}

func TestConnectProbe_BadAuth(t *testing.T) {
	addr := startFakeConnectProxy(t, basicAuth("u", "p"))
	u := mustUpstream(t, addr, "u", "wrong")

	if _, err := u.Probe(context.Background(), "connect"); err == nil {
		t.Fatal("want error on wrong proxy auth, got nil")
	}
}

func TestTCPProbe_Success(t *testing.T) {
	addr := startFakeConnectProxy(t, "")
	u := mustUpstream(t, addr, "", "")
	if _, err := u.Probe(context.Background(), "tcp"); err != nil {
		t.Fatalf("tcp probe failed: %v", err)
	}
}

func TestProbe_DeadProxyErrors(t *testing.T) {
	// Reserve a port then close it so the dial is refused.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	u := mustUpstream(t, addr, "u", "p")
	if _, err := u.Probe(context.Background(), "connect"); err == nil {
		t.Fatal("want error dialing dead proxy, got nil")
	}
}

func TestUpdateStatus_CircuitBreaker(t *testing.T) {
	addr := startFakeConnectProxy(t, "")
	u := mustUpstream(t, addr, "", "")

	if u.Status() != StatusUp {
		t.Fatal("new upstream should start Up")
	}
	for i := 0; i < 3; i++ {
		u.Stats.RecordHealthFailure()
	}
	u.UpdateStatus(3, 2)
	if u.Status() != StatusDown {
		t.Fatal("3 failures should trip to Down")
	}
	for i := 0; i < 2; i++ {
		u.Stats.RecordHealthSuccess(time.Millisecond)
	}
	u.UpdateStatus(3, 2)
	if u.Status() != StatusUp {
		t.Fatal("2 successes should recover to Up")
	}
}
