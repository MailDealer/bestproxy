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
	"syscall"
	"time"

	"github.com/elkin/bestproxy/internal/config"
	"github.com/elkin/bestproxy/internal/dashboard"
	"github.com/elkin/bestproxy/internal/health"
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
		upstreams := make([]*proxy.UpstreamProxy, 0, len(setConf.Proxies))
		for _, pc := range setConf.Proxies {
			fwdURL, err := pc.ProxyURL() // validated in config.Load
			if err != nil {
				logger.Error("parse proxy", "set", setConf.Name, "host", pc.Host, "err", err)
				os.Exit(1)
			}
			u := proxy.NewUpstream(setConf.Name, fwdURL, origin, setConf.Pool, cfg.TLS.InsecureSkipVerify)
			upstreams = append(upstreams, u)
			allUpstreams = append(allUpstreams, u)
			// fwdURL.Host only — never log userinfo (password).
			logger.Info("registered upstream", "set", setConf.Name, "proxy", fwdURL.Host,
				"origin", origin.String(), "pool_min", setConf.Pool.Min, "pool_max", setConf.Pool.Max)
		}
		pools = append(pools, proxy.NewPool(setConf.Name, upstreams))
	}

	// Start health checker.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker := health.New(cfg.Health, allUpstreams)
	checker.Start(ctx)

	// Pre-warm connection pools in background — don't block startup.
	for si, setConf := range cfg.Sets {
		min := setConf.Pool.Min
		for _, u := range pools[si].Upstreams {
			u := u
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
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-quit
		logger.Info("shutting down")
		cancel() // stop health checkers

		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Error("shutdown error", "err", err)
		}
	}()

	logger.Info("listening", "addr", cfg.Server.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
