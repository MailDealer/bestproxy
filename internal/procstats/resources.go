// Package procstats reports the process's own resource usage (CPU, memory, uptime)
// against the container's cgroup limits, so an operator can see per-pod headroom.
// Ported from the routerai gateway (core/internal/observability) to keep the JSON
// shape identical across both services.
package procstats

import (
	"bufio"
	"context"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CPU usage can't be measured instantly, so a background sampler records the
// process CPU time and cgroup throttling every sampleEvery; a snapshot diffs the
// oldest and newest samples (window ≈ (samples-1) × sampleEvery ≈ 10s).
const (
	sampleEvery = 5 * time.Second
	samples     = 3
)

// cgroup v2 root inside the container; v1 keeps memory/ and cpu/ subdirectories.
var cgroupRoot = "/sys/fs/cgroup"

// Limits are the pod's ceilings. MemoryLimitKB is the container limit (cgroup),
// falling back to GOMEMLIMIT; 0 = unknown. CPULimitCores is the cgroup quota,
// falling back to GOMAXPROCS.
type Limits struct {
	MemoryLimitKB uint64  `json:"memory_limit_kb"`
	CPULimitCores float64 `json:"cpu_limit_cores"`
}

// CPU is CPU usage over the last window. WindowSeconds = 0 means there are not
// enough samples yet (just started). ThrottledPercent is the share of CFS periods
// in the window where the cgroup hit its quota.
type CPU struct {
	UsageCores       float64 `json:"usage_cores"`
	WindowSeconds    float64 `json:"window_seconds"`
	ThrottledPercent float64 `json:"throttled_percent"`
}

type sample struct {
	at          time.Time
	cpuSeconds  float64
	nrPeriods   uint64
	nrThrottled uint64
}

// Sampler holds the latest CPU samples and the container limits (read once: pod
// limits don't change without a restart).
type Sampler struct {
	once   sync.Once
	limits Limits

	mu      sync.Mutex
	samples []sample
}

// Start runs the background sampler until ctx is cancelled. Repeated calls are no-ops.
func (r *Sampler) Start(ctx context.Context) {
	r.once.Do(func() {
		r.limits = readLimits()
		r.sample()
		go func() {
			t := time.NewTicker(sampleEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					r.sample()
				}
			}
		}()
	})
}

func (r *Sampler) sample() {
	s := sample{at: time.Now(), cpuSeconds: processCPUSeconds()}
	s.nrPeriods, s.nrThrottled = readCPUThrottling()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, s)
	if len(r.samples) > samples {
		r.samples = r.samples[len(r.samples)-samples:]
	}
}

// Snapshot returns the limits and the CPU usage over the available samples.
func (r *Sampler) Snapshot() (Limits, CPU) {
	r.mu.Lock()
	defer r.mu.Unlock()

	limits := r.limits
	if limits.CPULimitCores == 0 {
		limits.CPULimitCores = float64(runtime.GOMAXPROCS(0))
	}
	var cpu CPU
	if n := len(r.samples); n >= 2 {
		first, last := r.samples[0], r.samples[n-1]
		window := last.at.Sub(first.at).Seconds()
		if window > 0 {
			cpu.WindowSeconds = round2(window)
			cpu.UsageCores = round2((last.cpuSeconds - first.cpuSeconds) / window)
		}
		if periods := last.nrPeriods - first.nrPeriods; periods > 0 && last.nrPeriods >= first.nrPeriods {
			cpu.ThrottledPercent = round2(float64(last.nrThrottled-first.nrThrottled) / float64(periods) * 100)
		}
	}
	return limits, cpu
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// processCPUSeconds is the process user+system time. The container runs a single
// process, so this is the container's usage.
func processCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
}

func readLimits() Limits {
	var l Limits
	// cgroup v2, then v1. "No limit" values (max / huge number) are dropped.
	if b, ok := readUint(cgroupRoot + "/memory.max"); ok {
		l.MemoryLimitKB = b / 1024
	} else if b, ok := readUint(cgroupRoot + "/memory/memory.limit_in_bytes"); ok && b < 1<<60 {
		l.MemoryLimitKB = b / 1024
	}
	if l.MemoryLimitKB == 0 {
		if gml := debug.SetMemoryLimit(-1); gml > 0 && gml < math.MaxInt64 {
			l.MemoryLimitKB = uint64(gml) / 1024
		}
	}

	if f := readFields(cgroupRoot + "/cpu.max"); len(f) == 2 && f[0] != "max" {
		l.CPULimitCores = quotaCores(f[0], f[1])
	} else if q, p := readFields(cgroupRoot+"/cpu/cpu.cfs_quota_us"), readFields(cgroupRoot+"/cpu/cpu.cfs_period_us"); len(q) == 1 && len(p) == 1 && q[0] != "-1" {
		l.CPULimitCores = quotaCores(q[0], p[0])
	}
	return l
}

func quotaCores(quota, period string) float64 {
	q, err1 := strconv.ParseFloat(quota, 64)
	p, err2 := strconv.ParseFloat(period, 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0
	}
	return round2(q / p)
}

// readCPUThrottling returns nr_periods and nr_throttled from cpu.stat (v2, then v1).
func readCPUThrottling() (periods, throttled uint64) {
	for _, path := range []string{cgroupRoot + "/cpu.stat", cgroupRoot + "/cpu/cpu.stat"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) != 2 {
				continue
			}
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				continue
			}
			switch fields[0] {
			case "nr_periods":
				periods = v
			case "nr_throttled":
				throttled = v
			}
		}
		f.Close()
		return periods, throttled
	}
	return 0, 0
}

func readFields(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// readUint reads a single number from a file; "max" and garbage → ok=false.
func readUint(path string) (uint64, bool) {
	f := readFields(path)
	if len(f) != 1 {
		return 0, false
	}
	v, err := strconv.ParseUint(f[0], 10, 64)
	return v, err == nil
}
