package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// No per-request failover: exactly ONE upstream is attempted, even when it errors and
// other healthy upstreams exist. An error returns 502 without retrying anyone else.
func TestNoFailover_SingleAttempt(t *testing.T) {
	a := newFake("a", StatusUp, 10)
	a.rt = func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }
	b := newFake("b", StatusUp, 10)
	b.rt = func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }

	p := &Pool{Name: "t", sel: []upstream{a, b}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("y")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if a.reqs+b.reqs != 1 {
		t.Fatalf("no-failover: exactly one attempt total, got a=%d b=%d", a.reqs, b.reqs)
	}
	if a.errs+b.errs != 1 {
		t.Fatalf("want one recorded error, got a=%d b=%d", a.errs, b.errs)
	}
}

// Success path: body is streamed straight to the chosen origin, response streamed back.
func TestNoFailover_SuccessStreamsBody(t *testing.T) {
	a := newFake("a", StatusUp, 1)
	a.rt = func(*http.Request) (*http.Response, error) { return okResp(200, "OK"), nil }

	p := &Pool{Name: "t", sel: []upstream{a}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader("hello-body")))

	if rec.Code != 200 || rec.Body.String() != "OK" {
		t.Fatalf("want 200/OK, got %d/%q", rec.Code, rec.Body.String())
	}
	if string(a.gotBody) != "hello-body" {
		t.Fatalf("upstream got body %q, want hello-body", a.gotBody)
	}
	if a.reqs != 1 || a.oks != 1 {
		t.Fatalf("want reqs=1 oks=1, got %d/%d", a.reqs, a.oks)
	}
}

// A tunnel failure is marked unambiguously: X-Bestproxy-Error header (= kind) plus a
// stable JSON body, so the gateway can tell our 502 apart from a proxied origin response.
func TestTunnelError_MarkedResponse(t *testing.T) {
	a := newFake("fwd-nl-11.msndr.net:443", StatusUp, 10)
	a.rt = func(*http.Request) (*http.Response, error) {
		return nil, errors.New("read tcp 1.2.3.4:443: connection reset by peer")
	}
	p := &Pool{Name: "openrouter", sel: []upstream{a}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("y")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if got := rec.Header().Get(HeaderError); got != "stale" {
		t.Fatalf("X-Bestproxy-Error = %q, want stale", got)
	}
	if got := rec.Header().Get(HeaderUpstream); got != "fwd-nl-11.msndr.net:443" {
		t.Fatalf("X-Bestproxy-Upstream = %q", got)
	}
	if got := rec.Header().Get(HeaderSet); got != "openrouter" {
		t.Fatalf("X-Bestproxy-Set = %q", got)
	}

	var body struct{ Error tunnelError }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	e := body.Error
	if e.Source != "bestproxy" || e.Code != errCodeTunnelFailed || e.Kind != "stale" {
		t.Fatalf("unexpected error body: %+v", e)
	}
	if e.Message == "" || e.Detail == "" {
		t.Fatalf("want non-empty message+detail, got %+v", e)
	}
}

// The no-healthy-upstream case is marked too, with its own code/kind and no upstream addr.
func TestTunnelError_NoUpstreamMarked(t *testing.T) {
	a := newFake("a", StatusDown, 10)
	p := &Pool{Name: "openrouter", sel: []upstream{a}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if got := rec.Header().Get(HeaderError); got != kindNoUpstream {
		t.Fatalf("X-Bestproxy-Error = %q, want %q", got, kindNoUpstream)
	}
	if got := rec.Header().Get(HeaderUpstream); got != "" {
		t.Fatalf("no_upstream must not set upstream header, got %q", got)
	}
	var body struct{ Error tunnelError }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Error.Code != errCodeNoUpstream {
		t.Fatalf("code = %q, want %q", body.Error.Code, errCodeNoUpstream)
	}
}

// A down upstream is never attempted; with none healthy the pool returns 502.
func TestNoFailover_NoHealthyUpstream502(t *testing.T) {
	a := newFake("a", StatusDown, 10)
	a.rt = func(*http.Request) (*http.Response, error) { t.Fatal("down upstream must not be tried"); return nil, nil }

	p := &Pool{Name: "t", sel: []upstream{a}}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if a.reqs != 0 {
		t.Fatalf("want zero attempts on down upstream, got %d", a.reqs)
	}
}
