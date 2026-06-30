package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server ServerConfig `yaml:"server"`
	Health HealthConfig `yaml:"health"`
	Sets   []SetConfig  `yaml:"sets"`
}

type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

type HealthConfig struct {
	Interval          time.Duration `yaml:"interval"`
	Timeout           time.Duration `yaml:"timeout"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	RecoveryThreshold int           `yaml:"recovery_threshold"`
}

type PoolConfig struct {
	Min int `yaml:"min"` // connections to pre-warm per upstream at startup
	Max int `yaml:"max"` // MaxIdleConnsPerHost — cap on idle connections kept alive
}

type SetConfig struct {
	Name    string        `yaml:"name"`
	Pool    PoolConfig    `yaml:"pool"`
	Proxies []ProxyConfig `yaml:"proxies"`
	// Backup proxies are used only when every proxy in Proxies is down.
	Backup []ProxyConfig `yaml:"backup"`
}

type ProxyConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

func (p ProxyConfig) Addr() string {
	port := p.Port
	if port == 0 {
		port = 443
	}
	return fmt.Sprintf("%s:%d", p.Host, port)
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

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
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
	for i := range cfg.Sets {
		if cfg.Sets[i].Pool.Min == 0 {
			cfg.Sets[i].Pool.Min = 5
		}
		if cfg.Sets[i].Pool.Max == 0 {
			cfg.Sets[i].Pool.Max = 100
		}
	}
}

func validate(cfg *Config) error {
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
		if len(s.Proxies) == 0 {
			return fmt.Errorf("set %q has no proxies", s.Name)
		}
		for _, p := range s.Proxies {
			if p.Host == "" {
				return fmt.Errorf("set %q has proxy with empty host", s.Name)
			}
		}
		for _, p := range s.Backup {
			if p.Host == "" {
				return fmt.Errorf("set %q has backup proxy with empty host", s.Name)
			}
		}
	}
	return nil
}
