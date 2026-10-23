package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/elkin/bestproxy/internal/config"
	"github.com/elkin/bestproxy/internal/dashboard"
	"github.com/elkin/bestproxy/internal/health"
	"github.com/elkin/bestproxy/internal/procstats"
	"github.com/elkin/bestproxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	// Build pools and collect all upstreams for health checker.
	var allUpstreams []*proxy.UpstreamProxy
	pools := make([]*proxy.Pool, 0, len(cfg.Sets))

	for _, setConf := range cfg.Sets {
		origin, err := url.Parse(setConf.Origin) // validated in config.Load
		if err != nil {
			logger.Error("parse origin", "set", setConf.Name, "origin", setConf.Origin, "err", err)
			os.Exit(1)
		}
		upstreams := make([]*proxy.UpstreamProxy, 0, len(setConf.Proxies)+len(setConf.Backup))
		for _, pc := range setConf.Proxies {
			fwdURL, err := pc.ProxyURL() // validated in config.Load
			if err != nil {
				logger.Error("parse proxy", "set", setConf.Name, "host", pc.Host, "err", err)
				os.Exit(1)
			}
			u := proxy.NewUpstream(setConf.Name, fwdURL, origin, false, setConf.Pool, cfg.TLS.InsecureSkipVerify)
			upstreams = append(upstreams, u)
			allUpstreams = append(allUpstreams, u)
			// fwdURL.Host only — never log userinfo (password).
			logger.Info("registered upstream", "set", setConf.Name, "proxy", fwdURL.Host,
				"origin", origin.String(), "backup", false, "pool_min", setConf.Pool.Min, "pool_max", setConf.Pool.Max)
		}
		for _, pc := range setConf.Backup {
			fwdURL, err := pc.ProxyURL() // validated in config.Load
			if err != nil {
				logger.Error("parse backup proxy", "set", setConf.Name, "host", pc.Host, "err", err)
				os.Exit(1)
			}
			u := proxy.NewUpstream(setConf.Name, fwdURL, origin, true, setConf.Pool, cfg.TLS.InsecureSkipVerify)
			upstreams = append(upstreams, u)
			allUpstreams = append(allUpstreams, u)
			logger.Info("registered upstream", "set", setConf.Name, "proxy", fwdURL.Host,
				"origin", origin.String(), "backup", true, "pool_min", setConf.Pool.Min, "pool_max", setConf.Pool.Max)
		}
		pools = append(pools, proxy.NewPool(setConf.Name, upstreams))
	}

	// Start health checker.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker := health.New(cfg.Health, allUpstreams)
	checker.Start(ctx)

	// Process resource sampler (CPU over a ~10s window, cgroup limits) for /stats.
	sampler := &procstats.Sampler{}
	sampler.Start(ctx)

	// Pre-warm connection pools in background — don't block startup.
	for si, setConf := range cfg.Sets {
		min := setConf.Pool.Min
		for _, u := range pools[si].Upstreams {
			u := u
			if u.Backup {
				continue // reserves are not pre-warmed; only health-checked
			}
			go func() {
				logger.Info("warming up connections", "addr", u.Addr, "n", min)
				u.WarmUp(ctx, min)
				logger.Info("warm-up done", "addr", u.Addr)
			}()
		}
	}

	// Build dashboard handler.
	dash, err := dashboard.New(pools)
	if err != nil {
		logger.Error("build dashboard", "err", err)
		os.Exit(1)
	}

	// Build HTTP mux.
	mux := http.NewServeMux()

	for _, p := range pools {
		p := p
		pattern := "/" + p.Name + "/"
		mux.Handle(pattern, http.StripPrefix("/"+p.Name, p))
		logger.Info("registered endpoint", "path", pattern, "set", p.Name)
	}

	// Liveness vs readiness: during drain the process is alive (/healthz 200) but must
	// leave load balancing (/readyz 503) while it finishes in-flight requests.
	var draining atomic.Bool
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("/stats", procstats.NewCollector(sampler, pools))
	mux.HandleFunc("/dashboard/events", dash.ServeEvents)
	mux.HandleFunc("/dashboard/json", dash.ServeJSON)
	mux.HandleFunc("/dashboard", dash.ServeDashboard)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      closeWhenDraining(&draining, mux),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Shutdown does not cancel active requests; end dashboard SSE streams explicitly.
	srv.RegisterOnShutdown(dash.Close)

	// Graceful shutdown on SIGTERM/SIGINT:
	//  1. mark not-ready and keep serving for shutdown_delay, so the pod leaves the
	//     Service endpoints before we stop accepting connections;
	//  2. close the listener and wait up to shutdown_timeout for in-flight requests;
	//  3. only then stop background work (health checks keep running during drain).
	// A second signal aborts the drain immediately.
	quit := make(chan os.Signal, 2)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	shutdownDone := make(chan struct{})
	exitCode := 0 // set before shutdownDone closes; non-zero if the drain timed out

	go func() {
		defer close(shutdownDone)
		sig := <-quit
		draining.Store(true)
		logger.Info("draining: readiness off, still serving",
			"signal", sig.String(), "delay", cfg.Server.ShutdownDelay)
		go func() {
			sig := <-quit
			logger.Error("second signal, exiting without waiting for in-flight requests", "signal", sig.String())
			os.Exit(1)
		}()

		time.Sleep(cfg.Server.ShutdownDelay)

		logger.Info("shutting down: waiting for in-flight requests", "timeout", cfg.Server.ShutdownTimeout)
		start := time.Now()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Error("shutdown timed out, closing remaining connections", "err", err)
			srv.Close()
			exitCode = 1
		} else {
			logger.Info("all requests finished", "drain", time.Since(start).Round(time.Millisecond))
		}
		cancel() // stop health checkers, sampler, warm-up
	}()

	logger.Info("listening", "addr", cfg.Server.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for the drain to finish,
	// otherwise exiting main would kill in-flight requests.
	<-shutdownDone
	logger.Info("stopped", "exit_code", exitCode)
	os.Exit(exitCode)
}

// closeWhenDraining answers with "Connection: close" once draining starts. Removing the
// pod from Endpoints only affects NEW connections: clients (the gateway's keep-alive
// pool) keep sending requests over existing ones. Closing them per-response during
// shutdown_delay makes clients re-dial to other pods; otherwise Shutdown would close
// idle conns while a client is writing a non-retryable POST into them (EOF/reset).
func closeWhenDraining(draining *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}
