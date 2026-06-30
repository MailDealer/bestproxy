package proxy

import "math/rand/v2"

// Pick selects the best upstream using Power-of-Two-Choices with EWMA latency.
// Primary upstreams are always preferred; backup upstreams are used only when
// no primary upstream is up. Returns nil if all upstreams (primary and backup)
// are down.
func Pick(upstreams []*UpstreamProxy) *UpstreamProxy {
	if u := pickTier(upstreams, false); u != nil {
		return u
	}
	return pickTier(upstreams, true)
}

// pickTier runs P2C+EWMA selection over the healthy upstreams of a single tier
// (primary when backup is false, reserve when backup is true).
func pickTier(upstreams []*UpstreamProxy, backup bool) *UpstreamProxy {
	candidates := make([]*UpstreamProxy, 0, len(upstreams))
	for _, u := range upstreams {
		if u.Backup == backup && u.Status() == StatusUp {
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
