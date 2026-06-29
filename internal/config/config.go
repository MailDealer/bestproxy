package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Health   HealthConfig   `yaml:"health"`
	Failover FailoverConfig `yaml:"failover"`
	TLS      TLSConfig      `yaml:"tls"`
	Sets     []SetConfig    `yaml:"sets"`
}

type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

type HealthConfig struct {
	Mode              string        `yaml:"mode"` // "connect" (default) | "tcp"
	Interval          time.Duration `yaml:"interval"`
	Timeout           time.Duration `yaml:"timeout"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	RecoveryThreshold int           `yaml:"recovery_threshold"`
}

// FailoverConfig controls per-request retry across upstreams in a set.
type FailoverConfig struct {
	MaxAttempts    int   `yaml:"max_attempts"`     // max upstreams tried per request
	MaxBufferBytes int64 `yaml:"max_buffer_bytes"` // cap on buffered request body (for retry)
}

// TLSConfig is for the origin/proxy TLS handshakes. insecure_skip_verify is TEST ONLY.
type TLSConfig struct {
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type PoolConfig struct {
	Min int `yaml:"min"` // connections to pre-warm per upstream at startup
	Max int `yaml:"max"` // MaxIdleConnsPerHost — cap on idle connections kept alive
}

type SetConfig struct {
	Name    string        `yaml:"name"`
	Origin  string        `yaml:"origin"` // real upstream, e.g. https://openrouter.ai
	Pool    PoolConfig    `yaml:"pool"`
	Proxies []ProxyConfig `yaml:"proxies"`
}

// ProxyConfig is one forward proxy. Auth is embedded per-proxy in Host using the
// "login:password@hostname[:port]" form (env-expandable, e.g. "${FI01_CRED}@fwd-fi-01.msndr.net").
type ProxyConfig struct {
	Host   string `yaml:"host"`
	Port   int    `yaml:"port"`   // applied only if Host has no :port
	Scheme string `yaml:"scheme"` // "https" (default) | "http" (e2e tests only)
}

// ProxyURL parses Host into a forward-proxy URL with optional basic-auth userinfo.
func (p ProxyConfig) ProxyURL() (*url.URL, error) {
	raw := strings.TrimSpace(p.Host)
	if raw == "" {
		return nil, fmt.Errorf("empty proxy host")
	}

	scheme := p.Scheme
	if scheme == "" {
		scheme = "https"
	}

	var user *url.Userinfo
	hostport := raw
	// Split userinfo from host on the LAST '@' (passwords may contain '@').
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		cred := raw[:at]
		hostport = raw[at+1:]
		login, pass, hasPass := strings.Cut(cred, ":")
		if hasPass {
			user = url.UserPassword(login, pass)
		} else {
			user = url.User(login)
		}
	}

	if hostport == "" {
		return nil, fmt.Errorf("proxy %q: empty hostname", raw)
	}
	if !strings.Contains(hostport, ":") {
		port := p.Port
		if port == 0 {
			port = 443
		}
		hostport = fmt.Sprintf("%s:%d", hostport, port)
	}

	return &url.URL{Scheme: scheme, Host: hostport, User: user}, nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	expandEnv(&cfg)
	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// expandEnv resolves ${VAR} references in origin + per-proxy host (which carries auth) —
// not over the whole file, to avoid mangling '$' that appears in other values.
func expandEnv(cfg *Config) {
	for i := range cfg.Sets {
		cfg.Sets[i].Origin = resolveEnv(cfg.Sets[i].Origin)
		for j := range cfg.Sets[i].Proxies {
			cfg.Sets[i].Proxies[j].Host = resolveEnv(cfg.Sets[i].Proxies[j].Host)
		}
	}
}

func resolveEnv(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, os.Getenv)
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = ":8888"
	}
	if cfg.Server.ReadTimeout == 0 {
		cfg.Server.ReadTimeout = 30 * time.Second
	}
	if cfg.Server.WriteTimeout == 0 {
		cfg.Server.WriteTimeout = 30 * time.Second
	}
	if cfg.Server.IdleTimeout == 0 {
		cfg.Server.IdleTimeout = 90 * time.Second
	}
	if cfg.Health.Mode == "" {
		cfg.Health.Mode = "connect"
	}
	if cfg.Health.Interval == 0 {
		cfg.Health.Interval = 10 * time.Second
	}
	if cfg.Health.Timeout == 0 {
		cfg.Health.Timeout = 5 * time.Second
	}
	if cfg.Health.FailureThreshold == 0 {
		cfg.Health.FailureThreshold = 3
	}
	if cfg.Health.RecoveryThreshold == 0 {
		cfg.Health.RecoveryThreshold = 2
	}
	if cfg.Failover.MaxAttempts == 0 {
		cfg.Failover.MaxAttempts = 3
	}
	if cfg.Failover.MaxBufferBytes == 0 {
		cfg.Failover.MaxBufferBytes = 10 * 1024 * 1024 // 10 MiB
	}
	for i := range cfg.Sets {
		if cfg.Sets[i].Pool.Min == 0 {
			cfg.Sets[i].Pool.Min = 5
		}
		if cfg.Sets[i].Pool.Max == 0 {
			cfg.Sets[i].Pool.Max = 100
		}
		for j := range cfg.Sets[i].Proxies {
			if cfg.Sets[i].Proxies[j].Scheme == "" {
				cfg.Sets[i].Proxies[j].Scheme = "https"
			}
		}
	}
}

func validate(cfg *Config) error {
	if cfg.Health.Mode != "connect" && cfg.Health.Mode != "tcp" {
		return fmt.Errorf("health.mode must be \"connect\" or \"tcp\", got %q", cfg.Health.Mode)
	}
	if len(cfg.Sets) == 0 {
		return fmt.Errorf("no sets defined")
	}
	seen := make(map[string]bool)
	for _, s := range cfg.Sets {
		if s.Name == "" {
			return fmt.Errorf("set has empty name")
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate set name: %q", s.Name)
		}
		seen[s.Name] = true

		o, err := url.Parse(s.Origin)
		if err != nil || o.Scheme != "https" || o.Host == "" {
			return fmt.Errorf("set %q: origin must be a valid https URL, got %q", s.Name, s.Origin)
		}

		if len(s.Proxies) == 0 {
			return fmt.Errorf("set %q has no proxies", s.Name)
		}
		for _, p := range s.Proxies {
			if p.Scheme != "http" && p.Scheme != "https" {
				return fmt.Errorf("set %q proxy %q: scheme must be \"http\" or \"https\", got %q", s.Name, p.Host, p.Scheme)
			}
			if _, err := p.ProxyURL(); err != nil {
				return fmt.Errorf("set %q: %w", s.Name, err)
			}
		}
	}
	return nil
}
