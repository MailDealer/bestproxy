package proxy

import (
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
