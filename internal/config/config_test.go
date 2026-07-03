package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validBody = `
sets:
  - name: openrouter
    origin: https://openrouter.ai
    proxies:
      - host: ${FWD_CRED}@fwd-fi-01.msndr.net
`

func TestLoad_EnvExpandAndDefaults(t *testing.T) {
	t.Setenv("FWD_CRED", "alice:s3cr3t")

	cfg, err := Load(writeConfig(t, validBody))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sets[0].Proxies[0].Host != "alice:s3cr3t@fwd-fi-01.msndr.net" {
		t.Fatalf("env not expanded in host: %q", cfg.Sets[0].Proxies[0].Host)
	}
	if cfg.Health.Mode != "connect" {
		t.Fatalf("default health.mode want connect, got %q", cfg.Health.Mode)
	}
	if cfg.Sets[0].Proxies[0].Scheme != "https" {
		t.Fatalf("default proxy scheme want https, got %q", cfg.Sets[0].Proxies[0].Scheme)
	}
}

const backupBody = `
sets:
  - name: openrouter
    origin: https://openrouter.ai
    proxies:
      - host: ${FWD_CRED}@fwd-fi-01.msndr.net
    backup:
      - host: ${BK_CRED}@fwd-backup-01.msndr.net
`

func TestLoad_BackupExpandAndDefaults(t *testing.T) {
	t.Setenv("FWD_CRED", "alice:s3cr3t")
	t.Setenv("BK_CRED", "bob:r3serv3")

	cfg, err := Load(writeConfig(t, backupBody))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Sets[0].Backup) != 1 {
		t.Fatalf("want 1 backup, got %d", len(cfg.Sets[0].Backup))
	}
	if cfg.Sets[0].Backup[0].Host != "bob:r3serv3@fwd-backup-01.msndr.net" {
		t.Fatalf("env not expanded in backup host: %q", cfg.Sets[0].Backup[0].Host)
	}
	if cfg.Sets[0].Backup[0].Scheme != "https" {
		t.Fatalf("default backup scheme want https, got %q", cfg.Sets[0].Backup[0].Scheme)
	}
}

func TestProxyURL_ParsesHostAuth(t *testing.T) {
	cases := []struct {
		name           string
		p              ProxyConfig
		wantUser, wantPass, wantHost, wantScheme string
		wantNoUser     bool
	}{
		{name: "login:pass@host", p: ProxyConfig{Host: "u:p@h", Scheme: "https"},
			wantUser: "u", wantPass: "p", wantHost: "h:443", wantScheme: "https"},
		{name: "explicit port in host", p: ProxyConfig{Host: "u:p@h:8443", Scheme: "https"},
			wantUser: "u", wantPass: "p", wantHost: "h:8443", wantScheme: "https"},
		{name: "port field applied", p: ProxyConfig{Host: "u:p@h", Port: 9000, Scheme: "http"},
			wantUser: "u", wantPass: "p", wantHost: "h:9000", wantScheme: "http"},
		{name: "no auth", p: ProxyConfig{Host: "h", Scheme: "https"},
			wantHost: "h:443", wantScheme: "https", wantNoUser: true},
		{name: "password with @", p: ProxyConfig{Host: "u:p@ss@h", Scheme: "https"},
			wantUser: "u", wantPass: "p@ss", wantHost: "h:443", wantScheme: "https"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := c.p.ProxyURL()
			if err != nil {
				t.Fatalf("ProxyURL: %v", err)
			}
			if u.Scheme != c.wantScheme || u.Host != c.wantHost {
				t.Fatalf("got %s://%s, want %s://%s", u.Scheme, u.Host, c.wantScheme, c.wantHost)
			}
			if c.wantNoUser {
				if u.User != nil {
					t.Fatalf("want no userinfo, got %v", u.User)
				}
				return
			}
			if u.User.Username() != c.wantUser {
				t.Fatalf("user: got %q want %q", u.User.Username(), c.wantUser)
			}
			if pass, _ := u.User.Password(); pass != c.wantPass {
				t.Fatalf("pass: got %q want %q", pass, c.wantPass)
			}
		})
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := map[string]string{
		"missing origin": `
sets:
  - name: x
    proxies: [{host: u:p@fwd.test}]
`,
		"non-https origin": `
sets:
  - name: x
    origin: http://insecure.test
    proxies: [{host: u:p@fwd.test}]
`,
		"bad health mode": `
health: {mode: bogus}
sets:
  - name: x
    origin: https://ok.test
    proxies: [{host: u:p@fwd.test}]
`,
		"no proxies": `
sets:
  - name: x
    origin: https://ok.test
    proxies: []
`,
		"empty proxy host": `
sets:
  - name: x
    origin: https://ok.test
    proxies: [{host: ""}]
`,
		"empty backup host": `
sets:
  - name: x
    origin: https://ok.test
    proxies: [{host: u:p@fwd.test}]
    backup: [{host: ""}]
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}
