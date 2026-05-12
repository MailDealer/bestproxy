package proxy

import "net/http"

// Pool manages a named set of upstream proxies.
type Pool struct {
	Name      string
	Upstreams []*UpstreamProxy
}

func NewPool(name string, upstreams []*UpstreamProxy) *Pool {
	return &Pool{Name: name, Upstreams: upstreams}
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u := Pick(p.Upstreams)
	if u == nil {
		http.Error(w, "no healthy upstream available", http.StatusBadGateway)
		return
	}
	u.ServeHTTP(w, r)
}
