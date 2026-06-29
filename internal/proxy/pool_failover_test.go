package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPool(sel ...upstream) *Pool {
	return &Pool{Name: "t", sel: sel, maxAttempts: 3, maxBuffer: 1 << 20}
}

func TestFailover_FirstErrorsSecondSucceeds(t *testing.T) {
	first := newFake("first", StatusUp, 10)
	first.rt = func(*http.Request) (*http.Response, error) { return nil, errors.New("tunnel dead") }
	second := newFake("second", StatusUp, 20)
	second.rt = func(*http.Request) (*http.Response, error) { return okResp(200, "OK"), nil }

	p := newTestPool(first, second)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader("hello-body"))
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "OK" {
		t.Fatalf("want body OK, got %q", rec.Body.String())
	}
	// The buffered body must reach the upstream that actually served the request.
	if string(second.gotBody) != "hello-body" {
		t.Fatalf("second upstream got body %q, want hello-body", second.gotBody)
	}
	if first.errs != 1 || second.oks != 1 {
		t.Fatalf("want first.errs=1 second.oks=1, got %d/%d", first.errs, second.oks)
	}
}

func TestFailover_AllFail502(t *testing.T) {
	a := newFake("a", StatusUp, 10)
	a.rt = func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }
	b := newFake("b", StatusUp, 10)
	b.rt = func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }

	p := newTestPool(a, b)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("y")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if a.reqs != 1 || b.reqs != 1 {
		t.Fatalf("each upstream should be tried exactly once, got a=%d b=%d", a.reqs, b.reqs)
	}
}

func TestFailover_SuccessFirstNoRetry(t *testing.T) {
	a := newFake("a", StatusUp, 1) // lowest EWMA → always picked first
	a.rt = func(*http.Request) (*http.Response, error) { return okResp(201, "created"), nil }
	b := newFake("b", StatusUp, 100)
	b.rt = func(*http.Request) (*http.Response, error) { t.Fatal("second upstream must not be tried"); return nil, nil }

	p := newTestPool(a, b)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != 201 {
		t.Fatalf("want 201, got %d", rec.Code)
	}
	if a.reqs != 1 || b.reqs != 0 {
		t.Fatalf("want exactly one attempt on a, got a=%d b=%d", a.reqs, b.reqs)
	}
}

func TestFailover_ContextCanceledStops(t *testing.T) {
	a := newFake("a", StatusUp, 1) // lowest EWMA → always picked first
	a.rt = func(*http.Request) (*http.Response, error) { return nil, errors.New("dead") }
	b := newFake("b", StatusUp, 100)
	b.rt = func(*http.Request) (*http.Response, error) { t.Fatal("must stop after client cancel"); return nil, nil }

	p := newTestPool(a, b)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("y"))
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	cancel()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if a.reqs != 1 {
		t.Fatalf("want one attempt before cancel-stop, got %d", a.reqs)
	}
}

func TestFailover_BodyTooLarge413(t *testing.T) {
	a := newFake("a", StatusUp, 10)
	a.rt = func(*http.Request) (*http.Response, error) { t.Fatal("must not attempt oversized body"); return nil, nil }

	p := &Pool{Name: "t", sel: []upstream{a}, maxAttempts: 3, maxBuffer: 4}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("way too long body")))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rec.Code)
	}
	if a.reqs != 0 {
		t.Fatalf("want zero attempts, got %d", a.reqs)
	}
}
