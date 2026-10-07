package procstats

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/elkin/bestproxy/internal/proxy"
)

// Snapshot is a point-in-time view of this pod, served as JSON on /stats. The
// field names match the gateway's /gateway/stats so the admin UI can share code.
type Snapshot struct {
	Pod           string    `json:"pod"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	GoVersion     string    `json:"go_version"`
	GOMAXPROCS    int       `json:"gomaxprocs"`
	NumCPU        int       `json:"num_cpu"`
	Goroutines    int       `json:"goroutines"`

	Memory  Memory  `json:"memory"`
	Limits  Limits  `json:"limits"`
	CPU     CPU     `json:"cpu"`
	GC      GC      `json:"gc"`
	Traffic Traffic `json:"traffic"`
}

// Memory is process memory: rss_kb is the resident size from /proc (what the
// OOM killer compares to the limit), heap_* is the Go runtime heap.
type Memory struct {
	RSSKB       uint64 `json:"rss_kb"`
	HeapAllocKB uint64 `json:"heap_alloc_kb"`
	HeapInuseKB uint64 `json:"heap_inuse_kb"`
	HeapObjects uint64 `json:"heap_objects"`
	SysKB       uint64 `json:"sys_kb"`
	NextGCKB    uint64 `json:"next_gc_kb"`
}

type GC struct {
	NumGC         uint32  `json:"num_gc"`
	GCCPUFraction float64 `json:"gc_cpu_fraction"`
	LastPauseMS   float64 `json:"last_pause_ms"`
	PauseTotalMS  float64 `json:"pause_total_ms"`
}

// Traffic sums the upstream connection pools of this pod. Comparing in_flight
// across pods shows load skew (ClusterIP keepalive pins a gateway pod to one
// bestproxy pod).
type Traffic struct {
	InFlight      int64        `json:"in_flight"`
	PoolSize      int64        `json:"pool_size"`
	PoolIdle      int64        `json:"pool_idle"`
	TotalRequests int64        `json:"total_requests"`
	ErrorCount    int64        `json:"error_count"`
	Sets          []SetTraffic `json:"sets"`
}

type SetTraffic struct {
	Name          string `json:"name"`
	InFlight      int64  `json:"in_flight"`
	PoolSize      int64  `json:"pool_size"`
	PoolIdle      int64  `json:"pool_idle"`
	TotalRequests int64  `json:"total_requests"`
	ErrorCount    int64  `json:"error_count"`
}

// Collector builds snapshots for one process.
type Collector struct {
	startedAt time.Time
	pod       string
	sampler   *Sampler
	pools     []*proxy.Pool
}

// NewCollector expects sampler to be started by the caller.
func NewCollector(sampler *Sampler, pools []*proxy.Pool) *Collector {
	pod, _ := os.Hostname() // pod name in k8s
	return &Collector{startedAt: time.Now(), pod: pod, sampler: sampler, pools: pools}
}

func (c *Collector) Collect() Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	const kb = 1024
	snap := Snapshot{
		Pod:           c.pod,
		StartedAt:     c.startedAt,
		UptimeSeconds: int64(time.Since(c.startedAt).Seconds()),
		GoVersion:     runtime.Version(),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		NumCPU:        runtime.NumCPU(),
		Goroutines:    runtime.NumGoroutine(),
		Memory: Memory{
			RSSKB:       readRSSKB(),
			HeapAllocKB: ms.HeapAlloc / kb,
			HeapInuseKB: ms.HeapInuse / kb,
			HeapObjects: ms.HeapObjects,
			SysKB:       ms.Sys / kb,
			NextGCKB:    ms.NextGC / kb,
		},
		GC: GC{
			NumGC:         ms.NumGC,
			GCCPUFraction: ms.GCCPUFraction,
			LastPauseMS:   float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e6,
			PauseTotalMS:  float64(ms.PauseTotalNs) / 1e6,
		},
		Traffic: collectTraffic(c.pools),
	}
	snap.Limits, snap.CPU = c.sampler.Snapshot()
	return snap
}

func collectTraffic(pools []*proxy.Pool) Traffic {
	t := Traffic{Sets: make([]SetTraffic, 0, len(pools))}
	for _, p := range pools {
		st := SetTraffic{Name: p.Name}
		for _, u := range p.Upstreams {
			snap := u.Stats.Snapshot()
			st.InFlight += snap.Pool.InFlight
			st.PoolSize += snap.Pool.PoolSize
			st.PoolIdle += snap.Pool.Idle
			st.TotalRequests += snap.TotalRequests
			st.ErrorCount += snap.ErrorCount
		}
		t.InFlight += st.InFlight
		t.PoolSize += st.PoolSize
		t.PoolIdle += st.PoolIdle
		t.TotalRequests += st.TotalRequests
		t.ErrorCount += st.ErrorCount
		t.Sets = append(t.Sets, st)
	}
	return t
}

// ServeHTTP serves the snapshot as JSON.
func (c *Collector) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(c.Collect())
}

// readRSSKB reads VmRSS from /proc/self/status (Linux); 0 elsewhere or on error.
func readRSSKB() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line) // VmRSS: <kb> kB
		if len(fields) >= 2 {
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return v
			}
		}
		break
	}
	return 0
}
