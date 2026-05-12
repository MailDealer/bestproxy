package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

const ewmaAlpha = 0.2

// ProxyStats holds all metrics for one upstream proxy.
// Atomic fields must be first for 64-bit alignment on 32-bit platforms.
type ProxyStats struct {
	TotalRequests int64
	SuccessCount  int64
	ErrorCount    int64
	BytesProxied  int64
	LastCheckAt   int64 // unix nanos
	LastCheckOK   int32 // 1 = ok, 0 = fail
	ConsecFails   int32
	ConsecOK      int32

	ewmaMu sync.Mutex
	ewmaMs float64

	Window *RollingWindow
	Pool   *PoolStats
}

func New() *ProxyStats {
	return &ProxyStats{Window: &RollingWindow{}, Pool: &PoolStats{}}
}

func (s *ProxyStats) RecordRequest() {
	atomic.AddInt64(&s.TotalRequests, 1)
}

func (s *ProxyStats) RecordSuccess(bytes int64) {
	atomic.AddInt64(&s.SuccessCount, 1)
	if bytes > 0 {
		atomic.AddInt64(&s.BytesProxied, bytes)
	}
}

func (s *ProxyStats) RecordError() {
	atomic.AddInt64(&s.ErrorCount, 1)
}

func (s *ProxyStats) RecordHealthSuccess(latency time.Duration) {
	atomic.StoreInt64(&s.LastCheckAt, time.Now().UnixNano())
	atomic.StoreInt32(&s.LastCheckOK, 1)
	atomic.StoreInt32(&s.ConsecFails, 0)
	atomic.AddInt32(&s.ConsecOK, 1)

	ms := float64(latency.Milliseconds())
	s.ewmaMu.Lock()
	if s.ewmaMs == 0 {
		s.ewmaMs = ms
	} else {
		s.ewmaMs = ewmaAlpha*ms + (1-ewmaAlpha)*s.ewmaMs
	}
	s.ewmaMu.Unlock()

	s.Window.Add(latency)
}

func (s *ProxyStats) RecordHealthFailure() {
	atomic.StoreInt64(&s.LastCheckAt, time.Now().UnixNano())
	atomic.StoreInt32(&s.LastCheckOK, 0)
	atomic.StoreInt32(&s.ConsecOK, 0)
	atomic.AddInt32(&s.ConsecFails, 1)
}

func (s *ProxyStats) EWMA() float64 {
	s.ewmaMu.Lock()
	v := s.ewmaMs
	s.ewmaMu.Unlock()
	return v
}

func (s *ProxyStats) Snapshot() Snapshot {
	return Snapshot{
		TotalRequests: atomic.LoadInt64(&s.TotalRequests),
		SuccessCount:  atomic.LoadInt64(&s.SuccessCount),
		ErrorCount:    atomic.LoadInt64(&s.ErrorCount),
		BytesProxied:  atomic.LoadInt64(&s.BytesProxied),
		LastCheckAt:   atomic.LoadInt64(&s.LastCheckAt),
		LastCheckOK:   atomic.LoadInt32(&s.LastCheckOK) == 1,
		ConsecFails:   atomic.LoadInt32(&s.ConsecFails),
		EWMA:          s.EWMA(),
		Avg1m:         avg(s.Window, time.Minute),
		Avg5m:         avg(s.Window, 5*time.Minute),
		Avg1h:         avg(s.Window, time.Hour),
		Pool:          s.Pool.PoolSnapshot(),
	}
}

func avg(w *RollingWindow, d time.Duration) time.Duration {
	v, _ := w.AverageOver(d)
	return v
}

type Snapshot struct {
	TotalRequests int64
	SuccessCount  int64
	ErrorCount    int64
	BytesProxied  int64
	LastCheckAt   int64
	LastCheckOK   bool
	ConsecFails   int32
	EWMA          float64
	Avg1m         time.Duration
	Avg5m         time.Duration
	Avg1h         time.Duration
	Pool          PoolSnapshot
}
