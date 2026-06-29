package proxy

import (
	"math/rand/v2"
	"net/http"
	"net/url"
)

// upstream is the behavior Pool and the selector need. *UpstreamProxy implements it;
// tests inject fakes. Kept small so failover logic is unit-testable without real sockets.
type upstream interface {
	Status() Status
	EWMA() float64
	RoundTrip(*http.Request) (*http.Response, error)
	Origin() *url.URL
	RecordRequest()
	RecordSuccess(int64)
	RecordError()
}

// Pick selects the best upstream using Power-of-Two-Choices with EWMA latency.
// Returns nil if all upstreams are down.
func Pick(upstreams []upstream) upstream {
	return PickExcluding(upstreams, nil)
}

// PickExcluding is Pick but skips already-tried upstreams (for per-request failover).
func PickExcluding(upstreams []upstream, exclude map[upstream]bool) upstream {
	candidates := make([]upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if u.Status() == StatusUp && !exclude[u] {
			candidates = append(candidates, u)
		}
	}

	switch len(candidates) {
	case 0:
		return nil
	case 1:
		return candidates[0]
	}

	// Power-of-Two-Choices: pick two random candidates, return the one with lower EWMA.
	i := rand.IntN(len(candidates))
	j := rand.IntN(len(candidates) - 1)
	if j >= i {
		j++
	}
	a, b := candidates[i], candidates[j]

	if a.EWMA() <= b.EWMA() {
		return a
	}
	return b
}
