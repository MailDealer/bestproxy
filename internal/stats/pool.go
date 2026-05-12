package stats

import "sync/atomic"

// PoolStats tracks the live state of one upstream's HTTP connection pool.
// All fields are updated atomically — safe to read from any goroutine.
type PoolStats struct {
	TotalCreated int64 // total TLS connections ever dialed
	TotalClosed  int64 // total connections closed (dropped from pool or errored)
	InFlight     int64 // requests currently being processed
}

// PoolSize returns the number of connections currently alive (in-use + idle).
func (s *PoolStats) PoolSize() int64 {
	v := atomic.LoadInt64(&s.TotalCreated) - atomic.LoadInt64(&s.TotalClosed)
	if v < 0 {
		return 0
	}
	return v
}

// Idle returns the number of connections sitting idle in the pool.
func (s *PoolStats) Idle() int64 {
	idle := s.PoolSize() - atomic.LoadInt64(&s.InFlight)
	if idle < 0 {
		return 0
	}
	return idle
}

func (s *PoolStats) ConnCreated() { atomic.AddInt64(&s.TotalCreated, 1) }
func (s *PoolStats) ConnClosed()  { atomic.AddInt64(&s.TotalClosed, 1) }
func (s *PoolStats) ReqStart()    { atomic.AddInt64(&s.InFlight, 1) }
func (s *PoolStats) ReqDone()     { atomic.AddInt64(&s.InFlight, -1) }

type PoolSnapshot struct {
	InFlight     int64
	Idle         int64
	PoolSize     int64
	TotalCreated int64
}

func (s *PoolStats) PoolSnapshot() PoolSnapshot {
	return PoolSnapshot{
		InFlight:     atomic.LoadInt64(&s.InFlight),
		Idle:         s.Idle(),
		PoolSize:     s.PoolSize(),
		TotalCreated: atomic.LoadInt64(&s.TotalCreated),
	}
}
