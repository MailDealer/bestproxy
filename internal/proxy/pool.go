package proxy

import (
	"net/http"
	"net/url"
	"strings"
)

// Pool manages a named set of upstream forward proxies and routes each request to the
// best healthy one (Power-of-Two-Choices on EWMA latency). There is NO per-request
// failover: the request body is streamed straight to the origin, never buffered, so
// large multimodal payloads don't pin memory under high concurrency. Dead upstreams are
// avoided by the selector (health checker); an error mid-request returns 502 (no retry).
type Pool struct {
	Name      string
	Upstreams []*UpstreamProxy // concrete, for dashboard + health checker
	sel       []upstream       // interface view for selection
}

func NewPool(name string, upstreams []*UpstreamProxy) *Pool {
	sel := make([]upstream, len(upstreams))
	for i, u := range upstreams {
		sel[i] = u
	}
	return &Pool{Name: name, Upstreams: upstreams, sel: sel}
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u := Pick(p.sel)
	if u == nil {
		http.Error(w, "no healthy upstream available", http.StatusBadGateway)
		return
	}

	u.RecordRequest()
	resp, err := u.RoundTrip(buildOutbound(r, u.Origin()))
	if err != nil {
		u.RecordError(classifyErr(err))
		http.Error(w, "upstream failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	u.RecordSuccess(resp.ContentLength)
	copyResponse(w, resp)
}

// buildOutbound rewrites the inbound request to target the real origin. The /{set}
// prefix is already stripped by http.StripPrefix in the mux, so URL.Path is the origin
// path. The body carried over by Clone is streamed straight to the origin — never
// buffered — so multimodal payloads don't accumulate in memory.
func buildOutbound(r *http.Request, origin *url.URL) *http.Request {
	out := r.Clone(r.Context())
	out.URL.Scheme = origin.Scheme
	out.URL.Host = origin.Host
	out.Host = origin.Host // Host header = real origin → correct SNI/routing at CF
	out.RequestURI = ""    // must be empty for client requests

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
