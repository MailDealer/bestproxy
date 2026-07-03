package proxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/elkin/bestproxy/internal/stats"
)

// Response markers that unambiguously identify a bestproxy-generated tunnel failure,
// so a downstream gateway can tell OUR error apart from a real response proxied back
// from the origin (which never carries these). Treat the header names and the JSON
// body's "code"/"kind" values as a stable contract.
const (
	HeaderError    = "X-Bestproxy-Error"    // presence marks our error; value = kind
	HeaderUpstream = "X-Bestproxy-Upstream" // forward-proxy host:port (empty for no_upstream)
	HeaderSet      = "X-Bestproxy-Set"      // set name

	errCodeTunnelFailed = "tunnel_failed" // RoundTrip through the chosen upstream failed
	errCodeNoUpstream   = "no_upstream"   // no healthy upstream in the set to even try
	kindNoUpstream      = "no_upstream"   // synthetic kind for the no-upstream case
)

// errKindMessage maps a transport-failure kind to a human-readable, gateway-friendly
// sentence. Keeps the opaque Go error out of the primary message (it goes in "detail").
var errKindMessage = map[stats.ErrKind]string{
	stats.ErrKindStale:    "reused keepalive connection was closed by the origin before the request completed",
	stats.ErrKindTimeout:  "timed out establishing or using the tunnel to the origin",
	stats.ErrKindDial:     "could not connect to the forward proxy",
	stats.ErrKindTLS:      "TLS handshake with the tunnel failed",
	stats.ErrKindCanceled: "request was canceled before the tunnel completed",
	stats.ErrKindOther:    "tunnel request to the origin failed",
}

// tunnelError is the JSON body of a bestproxy-generated 502.
type tunnelError struct {
	Source   string `json:"source"`             // always "bestproxy"
	Code     string `json:"code"`               // tunnel_failed | no_upstream
	Kind     string `json:"kind"`               // stale|timeout|dial|tls|canceled|other|no_upstream
	Set      string `json:"set"`                // proxy set name
	Upstream string `json:"upstream,omitempty"` // forward-proxy host:port (omitted for no_upstream)
	Message  string `json:"message"`            // human-readable explanation
	Detail   string `json:"detail,omitempty"`   // raw Go error (diagnostics; may change)
}

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
		p.writeTunnelError(w, tunnelError{
			Code:    errCodeNoUpstream,
			Kind:    kindNoUpstream,
			Message: "no healthy upstream proxy available in set " + p.Name,
		})
		return
	}

	u.RecordRequest()
	resp, err := u.RoundTrip(buildOutbound(r, u.Origin()))
	if err != nil {
		kind := classifyErr(err)
		u.RecordError(kind)
		p.writeTunnelError(w, tunnelError{
			Code:     errCodeTunnelFailed,
			Kind:     kind.String(),
			Upstream: u.HostAddr(),
			Message:  errKindMessage[kind],
			Detail:   err.Error(),
		})
		return
	}

	u.RecordSuccess(resp.ContentLength)
	copyResponse(w, resp)
}

// writeTunnelError renders a 502 that unambiguously identifies a bestproxy tunnel
// failure: marker headers plus a stable JSON body. Downstream callers should branch on
// the X-Bestproxy-Error header (or the body's "code"/"kind"), not on the free text.
func (p *Pool) writeTunnelError(w http.ResponseWriter, te tunnelError) {
	te.Source = "bestproxy"
	te.Set = p.Name

	w.Header().Set(HeaderError, te.Kind)
	if te.Upstream != "" {
		w.Header().Set(HeaderUpstream, te.Upstream)
	}
	w.Header().Set(HeaderSet, te.Set)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)

	//nolint:errcheck // best-effort error body; nothing useful to do if the client is gone
	json.NewEncoder(w).Encode(struct {
		Error tunnelError `json:"error"`
	}{te})
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
