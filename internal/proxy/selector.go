package proxy

import "math/rand/v2"

// Pick selects the best upstream using Power-of-Two-Choices with EWMA latency.
// Returns nil if all upstreams are down.
func Pick(upstreams []*UpstreamProxy) *UpstreamProxy {
	candidates := make([]*UpstreamProxy, 0, len(upstreams))
	for _, u := range upstreams {
		if u.Status() == StatusUp {
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

	if a.Stats.EWMA() <= b.Stats.EWMA() {
		return a
	}
	return b
}
