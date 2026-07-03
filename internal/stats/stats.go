package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

const ewmaAlpha = 0.2

// ErrKind is a coarse classification of a transport failure, used for the dashboard
// errors-by-type breakdown. The proxy layer maps a raw RoundTrip error to one of these
// (see proxy.classifyErr); every kind surfaced as a 502 to the client (no retry).
type ErrKind int

const (
	ErrKindStale    ErrKind = iota // reused idle keepalive conn closed by the peer (EOF/reset/broken pipe)
	ErrKindTimeout                 // dial/TLS/response deadline exceeded
	ErrKindDial                    // could not establish the tunnel to the forward proxy
	ErrKindTLS                     // TLS handshake failure (to proxy or, in-tunnel, to origin)
	ErrKindCanceled                // client/request context canceled (caller went away)
	ErrKindOther                   // anything not matched above
	numErrKinds
)

// errKindLabels are the short human labels shown in the dashboard tooltip.
var errKindLabels = [numErrKinds]string{"stale", "timeout", "dial", "tls", "canceled", "other"}

// ProxyStats holds all metrics for one upstream proxy.
// Atomic fields must be first for 64-bit alignment on 32-bit platforms.
type ProxyStats struct {
	TotalRequests int64
	SuccessCount  int64
	ErrorCount    int64
	BytesProxied  int64
	errByKind     [numErrKinds]int64 // per-kind error counters, indexed by ErrKind
	LastCheckAt   int64              // unix nanos
	LastCheckOK   int32              // 1 = ok, 0 = fail
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

func (s *ProxyStats) RecordError(kind ErrKind) {
	atomic.AddInt64(&s.ErrorCount, 1)
	if kind < 0 || kind >= numErrKinds {
		kind = ErrKindOther
	}
	atomic.AddInt64(&s.errByKind[kind], 1)
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
	var errByKind [numErrKinds]int64
	for i := range errByKind {
		errByKind[i] = atomic.LoadInt64(&s.errByKind[i])
	}
	return Snapshot{
		TotalRequests: atomic.LoadInt64(&s.TotalRequests),
		SuccessCount:  atomic.LoadInt64(&s.SuccessCount),
		ErrorCount:    atomic.LoadInt64(&s.ErrorCount),
		BytesProxied:  atomic.LoadInt64(&s.BytesProxied),
		ErrByKind:     errByKind,
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
	ErrByKind     [numErrKinds]int64
	LastCheckAt   int64
	LastCheckOK   bool
	ConsecFails   int32
	EWMA          float64
	Avg1m         time.Duration
	Avg5m         time.Duration
	Avg1h         time.Duration
	Pool          PoolSnapshot
}

// ErrKindCount is one labeled bucket of the error breakdown.
type ErrKindCount struct {
	Label string
	Count int64
}

// ErrBreakdown returns the non-zero error buckets in declared ErrKind order,
// for rendering the dashboard tooltip.
func (s Snapshot) ErrBreakdown() []ErrKindCount {
	out := make([]ErrKindCount, 0, numErrKinds)
	for i := 0; i < int(numErrKinds); i++ {
		if s.ErrByKind[i] > 0 {
			out = append(out, ErrKindCount{Label: errKindLabels[i], Count: s.ErrByKind[i]})
		}
	}
	return out
}
