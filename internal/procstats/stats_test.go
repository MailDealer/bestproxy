package procstats

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/elkin/bestproxy/internal/config"
	"github.com/elkin/bestproxy/internal/proxy"
)

func TestQuotaCores(t *testing.T) {
	cases := []struct {
		quota, period string
		want          float64
	}{
		{"50000", "100000", 0.5},
		{"200000", "100000", 2},
		{"-1", "100000", 0},
		{"abc", "100000", 0},
		{"50000", "0", 0},
	}
	for _, c := range cases {
		if got := quotaCores(c.quota, c.period); got != c.want {
			t.Errorf("quotaCores(%q, %q) = %v, want %v", c.quota, c.period, got, c.want)
		}
	}
}

func withCgroupRoot(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev := cgroupRoot
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = prev })
}

func TestReadLimitsCgroupV2(t *testing.T) {
	withCgroupRoot(t, map[string]string{
		"memory.max": "536870912\n",
		"cpu.max":    "150000 100000\n",
	})
	l := readLimits()
	if l.MemoryLimitKB != 512*1024 {
		t.Errorf("MemoryLimitKB = %d, want %d", l.MemoryLimitKB, 512*1024)
	}
	if l.CPULimitCores != 1.5 {
		t.Errorf("CPULimitCores = %v, want 1.5", l.CPULimitCores)
	}
}

func TestReadLimitsUnlimited(t *testing.T) {
	withCgroupRoot(t, map[string]string{
		"memory.max": "max\n",
		"cpu.max":    "max 100000\n",
	})
	l := readLimits()
	if l.MemoryLimitKB != 0 || l.CPULimitCores != 0 {
		t.Errorf("unlimited cgroup: got %+v, want zero limits", l)
	}
}

func TestReadLimitsCgroupV1(t *testing.T) {
	withCgroupRoot(t, map[string]string{
		"memory/memory.limit_in_bytes": "268435456\n",
		"cpu/cpu.cfs_quota_us":         "50000\n",
		"cpu/cpu.cfs_period_us":        "100000\n",
	})
	l := readLimits()
	if l.MemoryLimitKB != 256*1024 || l.CPULimitCores != 0.5 {
		t.Errorf("v1 limits: got %+v", l)
	}
}

func TestReadCPUThrottling(t *testing.T) {
	withCgroupRoot(t, map[string]string{
		"cpu.stat": "usage_usec 100\nnr_periods 40\nnr_throttled 10\nthrottled_usec 5\n",
	})
	periods, throttled := readCPUThrottling()
	if periods != 40 || throttled != 10 {
		t.Errorf("throttling = %d/%d, want 40/10", periods, throttled)
	}
}

func TestSnapshotFallsBackToGOMAXPROCS(t *testing.T) {
	withCgroupRoot(t, nil)
	var s Sampler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	limits, cpu := s.Snapshot()
	if limits.CPULimitCores != float64(runtime.GOMAXPROCS(0)) {
		t.Errorf("CPULimitCores = %v, want GOMAXPROCS", limits.CPULimitCores)
	}
	if cpu.WindowSeconds != 0 {
		t.Errorf("single sample must not report a window, got %v", cpu.WindowSeconds)
	}
}

func TestServeHTTP(t *testing.T) {
	withCgroupRoot(t, nil)
	fwd, _ := url.Parse("http://user:pass@fwd.example:3128")
	origin, _ := url.Parse("https://origin.example")
	u := proxy.NewUpstream("openai", fwd, origin, false, config.PoolConfig{Min: 1, Max: 10}, false)
	u.Stats.Pool.ReqStart()
	u.RecordRequest()

	var s Sampler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	c := NewCollector(&s, []*proxy.Pool{proxy.NewPool("openai", []*proxy.UpstreamProxy{u})})
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var snap Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Goroutines == 0 || snap.Memory.HeapAllocKB == 0 {
		t.Errorf("runtime stats missing: %+v", snap)
	}
	if snap.StartedAt.IsZero() {
		t.Error("started_at is zero")
	}
	if snap.Traffic.InFlight != 1 || snap.Traffic.TotalRequests != 1 {
		t.Errorf("traffic totals = %+v", snap.Traffic)
	}
	if len(snap.Traffic.Sets) != 1 || snap.Traffic.Sets[0].Name != "openai" || snap.Traffic.Sets[0].InFlight != 1 {
		t.Errorf("traffic sets = %+v", snap.Traffic.Sets)
	}
}
