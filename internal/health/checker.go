package health

import (
	"context"
	"net"
	"time"

	"github.com/elkin/bestproxy/internal/config"
	"github.com/elkin/bestproxy/internal/proxy"
)

// Checker runs one goroutine per upstream proxy performing TCP health checks.
type Checker struct {
	cfg       config.HealthConfig
	upstreams []*proxy.UpstreamProxy
}

func New(cfg config.HealthConfig, upstreams []*proxy.UpstreamProxy) *Checker {
	return &Checker{cfg: cfg, upstreams: upstreams}
}

func (c *Checker) Start(ctx context.Context) {
	for _, u := range c.upstreams {
		go c.runLoop(ctx, u)
	}
}

func (c *Checker) runLoop(ctx context.Context, u *proxy.UpstreamProxy) {
	// Run an initial check immediately so EWMA is populated before first request.
	c.check(ctx, u)

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check(ctx, u)
		}
	}
}

func (c *Checker) check(ctx context.Context, u *proxy.UpstreamProxy) {
	checkCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(checkCtx, "tcp", u.Addr)
	latency := time.Since(start)

	if err != nil {
		u.Stats.RecordHealthFailure()
	} else {
		conn.Close()
		u.Stats.RecordHealthSuccess(latency)
	}

	u.UpdateStatus(c.cfg.FailureThreshold, c.cfg.RecoveryThreshold)
}
