package stats

import (
	"sync"
	"time"
)

const windowSize = 512

type Sample struct {
	At      int64
	Latency int64
}

// RollingWindow is a fixed-size circular buffer of latency samples.
// Three time windows (1m/5m/1h) are computed as filtered views on read.
type RollingWindow struct {
	mu    sync.RWMutex
	buf   [windowSize]Sample
	head  int
	count int
}

func (w *RollingWindow) Add(latency time.Duration) {
	w.mu.Lock()
	w.buf[w.head] = Sample{
		At:      time.Now().UnixNano(),
		Latency: int64(latency),
	}
	w.head = (w.head + 1) % windowSize
	if w.count < windowSize {
		w.count++
	}
	w.mu.Unlock()
}

// AverageOver returns mean latency over samples within duration d.
func (w *RollingWindow) AverageOver(d time.Duration) (avg time.Duration, n int) {
	cutoff := time.Now().Add(-d).UnixNano()
	w.mu.RLock()
	defer w.mu.RUnlock()

	var sum int64
	for i := 0; i < w.count; i++ {
		idx := (w.head - 1 - i + windowSize) % windowSize
		s := w.buf[idx]
		if s.At < cutoff {
			break
		}
		sum += s.Latency
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return time.Duration(sum / int64(n)), n
}
