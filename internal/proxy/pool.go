package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var errBodyTooLarge = errors.New("request body exceeds buffer limit")

// Pool manages a named set of upstream forward proxies and does per-request failover.
type Pool struct {
	Name      string
	Upstreams []*UpstreamProxy // concrete, for dashboard + health checker

	maxAttempts int
	maxBuffer   int64
	sel         []upstream // interface view for selection/failover
}

func NewPool(name string, upstreams []*UpstreamProxy, maxAttempts int, maxBuffer int64) *Pool {
	sel := make([]upstream, len(upstreams))
	for i, u := range upstreams {
		sel[i] = u
	}
	return &Pool{Name: name, Upstreams: upstreams, maxAttempts: maxAttempts, maxBuffer: maxBuffer, sel: sel}
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Buffer the body once so a failed attempt can be retried on another upstream.
	body, err := readAllLimited(r.Body, p.maxBuffer)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request too large to buffer", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		}
		return
	}

	tried := make(map[upstream]bool, p.maxAttempts)
	var lastErr error

	for attempt := 0; attempt < p.maxAttempts; attempt++ {
		u := PickExcluding(p.sel, tried)
		if u == nil {
			break
		}
		tried[u] = true

		u.RecordRequest()
		resp, err := u.RoundTrip(buildOutbound(r, u.Origin(), body))
		if err != nil {
			u.RecordError()
			lastErr = err
			if r.Context().Err() != nil {
				break // client gone — don't keep retrying
			}
			continue // tunnel/dial/TLS/h2-PING error before any byte — try next upstream
		}

		// Response received: headers not yet written to client until copyResponse,
		// but we are now committed to this upstream (streaming may have started).
		u.RecordSuccess(resp.ContentLength)
		copyResponse(w, resp)
		return
	}

	if lastErr == nil {
		lastErr = errors.New("no healthy upstream available")
	}
	http.Error(w, "all upstreams failed: "+lastErr.Error(), http.StatusBadGateway)
}

// buildOutbound rewrites the inbound request to target the real origin. The /{set}
// prefix is already stripped by http.StripPrefix in the mux, so URL.Path is the origin
// path. Body is a fresh reader each attempt (with GetBody) so retries re-send it.
func buildOutbound(r *http.Request, origin *url.URL, body []byte) *http.Request {
	out := r.Clone(r.Context())
	out.URL.Scheme = origin.Scheme
	out.URL.Host = origin.Host
	out.Host = origin.Host // Host header = real origin → correct SNI/routing at CF
	out.RequestURI = ""    // must be empty for client requests

	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	} else {
		out.Body = http.NoBody
		out.ContentLength = 0
		out.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
	}

	removeHopByHop(out.Header)
	out.Header.Del("X-Forwarded-For")
	return out
}

// copyResponse streams the upstream response back to the client, flushing per chunk
// so SSE/streaming responses are not buffered.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	dst := w.Header()
	for k, vv := range resp.Header {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// readAllLimited reads up to limit bytes; returns errBodyTooLarge if exceeded. A nil or
// empty body returns (nil, nil).
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if r == nil || r == http.NoBody {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	if len(data) == 0 {
		return nil, nil
	}
	return data, nil
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopByHop(h http.Header) {
	for _, c := range h["Connection"] {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}
