// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fixture is a config directory holding a token file, a secret file and the
// config itself, so every test starts from something that would actually load.
type fixture struct {
	t      *testing.T
	dir    string
	token  string
	secret string
	config string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir}
	// Deliberately padded: Load trims both, the way a mounted secret file ends
	// in a newline.
	f.token = f.write("token", "  hooks-token\n")
	f.secret = f.write("secret", "webhook-secret\n")
	f.config = filepath.Join(dir, "hooks.toml")
	return f
}

func (f *fixture) write(name, content string) string {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// valid is the smallest config that loads.
func (f *fixture) valid() string {
	return fmt.Sprintf(`
[daemon]
url = "https://rafiki.example.com"
token_file = %q

[[route]]
path = "/hooks/github"
source = "github"
secret_file = %q
target = "oncall"
[[route.rule]]
event = "workflow_run"
key = "github/{repository.full_name}"
`, f.token, f.secret)
}

// load writes body as the config file and loads it.
func (f *fixture) load(body string) (*Config, error) {
	f.t.Helper()
	if err := os.WriteFile(f.config, []byte(body), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", f.config, err)
	}
	return Load(f.config)
}

// mutate loads the valid config with one substitution applied.
func (f *fixture) mutate(t *testing.T, old, replacement string) (*Config, error) {
	t.Helper()
	body := strings.Replace(f.valid(), old, replacement, 1)
	return f.load(body)
}

func TestConfigLoadsAndAppliesDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)

	cfg, err := f.load(f.valid())
	c.NoError(err)
	c.Eq("https://rafiki.example.com", cfg.Daemon.URL)
	c.Eq(":8080", cfg.Daemon.Listen)
	c.Eq(10, cfg.Daemon.TimeoutSeconds)
	c.Eq(10*time.Second, cfg.timeout())
	c.Eq("hooks-token", cfg.token, "the token file is trimmed")
	c.Len(cfg.Routes, 1)

	route := cfg.Routes[0]
	c.Eq("/hooks/github", route.Path)
	c.Eq("github", route.Source)
	c.Eq("oncall", route.Target)
	c.Eq("github", route.Notify, "notify_source defaults to source")
	c.Eq(int64(1<<20), route.MaxBody)
	c.Eq("webhook-secret", string(route.secret), "the secret file is trimmed")
	c.Len(route.Rules, 1)
	c.Eq("workflow_run", route.Rules[0].Event)
	c.Eq("github/{repository.full_name}", route.Rules[0].Key)
}

func TestConfigExplicitValues(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)

	body := fmt.Sprintf(`
[daemon]
url = "http://127.0.0.1:8036"
token_file = %q
listen = "127.0.0.1:9090"
timeout_seconds = 3

[[route]]
path = "/hooks/omada"
source = "generic"
secret_file = %q
header = "X-Omada-Secret"
target = "network-ops"
notify_source = "omada"
max_body = 2048
[[route.rule]]
match = { event = "client_offline" }
fields = { mac = "client.mac" }
`, f.token, f.secret)

	cfg, err := f.load(body)
	c.NoError(err)
	c.Eq("127.0.0.1:9090", cfg.Daemon.Listen)
	c.Eq(3*time.Second, cfg.timeout())
	c.Eq("omada", cfg.Routes[0].Notify)
	c.Eq(int64(2048), cfg.Routes[0].MaxBody)
	c.Eq("X-Omada-Secret", cfg.Routes[0].Header)
	c.Eq("client_offline", cfg.Routes[0].Rules[0].Match["event"])
}

func TestConfigRejects(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		load func(t *testing.T) (*Config, error)
		want string
	}{
		{
			name: "missing secret file",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, f.secret, filepath.Join(f.dir, "not-there"))
			},
			want: "secret_file",
		},
		{
			name: "empty secret file",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				empty := f.write("empty-secret", "  \n")
				return f.mutate(t, f.secret, empty)
			},
			want: "is empty",
		},
		{
			name: "missing token file",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, f.token, filepath.Join(f.dir, "no-token"))
			},
			want: "token_file",
		},
		{
			name: "duplicate path",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				body := fmt.Sprintf(`
[daemon]
url = "https://rafiki.example.com"
token_file = %q

[[route]]
path = "/hooks/github"
source = "github"
secret_file = %q
target = "oncall"
[[route.rule]]
event = "workflow_run"

[[route]]
path = "/hooks/github"
source = "github"
secret_file = %q
target = "other"
[[route.rule]]
event = "push"
`, f.token, f.secret, f.secret)
				return f.load(body)
			},
			want: "path is used more than once",
		},
		{
			name: "unknown source",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `source = "github"`, `source = "bitbucket"`)
			},
			want: "unknown source",
		},
		{
			name: "unknown key",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `target = "oncall"`, "target = \"oncall\"\nmax_boddy = 10")
			},
			want: "max_boddy",
		},
		{
			name: "generic without a header",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `source = "github"`, `source = "generic"`)
			},
			want: "header is required for source generic",
		},
		{
			name: "half-set TLS pair",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `url = "https://rafiki.example.com"`, "url = \"https://rafiki.example.com\"\ntls_cert = \"/tmp/cert.pem\"")
			},
			want: "tls_cert and daemon.tls_key must be set together",
		},
		{
			name: "cleartext to a remote daemon",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, "https://rafiki.example.com", "http://rafiki.example.com")
			},
			want: "must be https unless the host is 127.0.0.1 or localhost",
		},
		{
			name: "no route",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				var b strings.Builder
				b.WriteString("[daemon]\n")
				b.WriteString("url = \"https://rafiki.example.com\"\n")
				fmt.Fprintf(&b, "token_file = %q\n", f.token)
				return f.load(b.String())
			},
			want: "at least one route is required",
		},
		{
			name: "route without a rule",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				body := strings.Replace(f.valid(), "[[route.rule]]\nevent = \"workflow_run\"\nkey = \"github/{repository.full_name}\"\n", "", 1)
				return f.load(body)
			},
			want: "at least one rule is required",
		},
		{
			name: "generic rule carrying an event",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `source = "github"`, "source = \"generic\"\nheader = \"X-Secret\"")
			},
			want: "event must be empty for source generic",
		},
		{
			name: "header on a signature scheme",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `target = "oncall"`, "target = \"oncall\"\nheader = \"X-Secret\"")
			},
			want: "header is only valid for source generic",
		},
		{
			name: "path without a leading slash",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `path = "/hooks/github"`, `path = "hooks/github"`)
			},
			want: "path must start with",
		},
		{
			name: "notify_source that the daemon would refuse",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `target = "oncall"`, "target = \"oncall\"\nnotify_source = \"GitHub Webhooks\"")
			},
			want: "must match",
		},
		{
			// The daemon's charset is lowercase-only, so an uppercase
			// notify_source is refused by the same rule the daemon applies.
			name: "uppercase notify_source",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `target = "oncall"`, "target = \"oncall\"\nnotify_source = \"GitHub\"")
			},
			want: "invalid source",
		},
		{
			// The mux answers /healthz itself, so this route would never see a
			// webhook: refuse it rather than let the gateway look configured.
			name: "route on the health path",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `path = "/hooks/github"`, `path = "/healthz"`)
			},
			want: "the health endpoint owns this path",
		},
		{
			name: "route path with a double slash",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `path = "/hooks/github"`, `path = "/hooks//github"`)
			},
			want: "path is not clean",
		},
		{
			name: "route path with a trailing slash",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `path = "/hooks/github"`, `path = "/hooks/github/"`)
			},
			want: "path is not clean",
		},
		{
			name: "route path with a dot segment",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `path = "/hooks/github"`, `path = "/hooks/./github"`)
			},
			want: "path is not clean",
		},
		{
			name: "route path with a parent segment",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, `path = "/hooks/github"`, `path = "/hooks/../github"`)
			},
			want: "path is not clean",
		},
		{
			// Past the server's WriteTimeout the response outlives the sender's
			// connection, so an accepted notification is retried into a
			// duplicate.
			name: "timeout at the write timeout",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, "token_file = ", fmt.Sprintf("timeout_seconds = %d\ntoken_file = ", writeTimeoutSeconds))
			},
			want: "must be under the",
		},
		{
			name: "timeout past the write timeout",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, "token_file = ", fmt.Sprintf("timeout_seconds = %d\ntoken_file = ", writeTimeoutSeconds+60))
			},
			want: "must be under the",
		},
		{
			name: "negative timeout",
			load: func(t *testing.T) (*Config, error) {
				t.Helper()
				return f.mutate(t, "token_file = ", "timeout_seconds = -1\ntoken_file = ")
			},
			want: "must be positive",
		},
	}
	for _, tc := range cases {
		load := tc.load
		want := tc.want
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			_, err := load(t)
			c.Error(err)
			c.ErrorContains(err, want)
		})
	}
}

// TestRouteValidateNotifySource pins that notify_source is checked with
// notify.ValidSource — the daemon's own rule — rather than a copy of it. An
// empty notify_source never reaches validate through Load (it defaults to
// source), so the empty case calls validate directly.
func TestRouteValidateNotifySource(t *testing.T) {
	base := Route{
		Path:       "/hooks/github",
		Source:     "github",
		SecretFile: "/run/secrets/secret",
		Target:     "oncall",
		MaxBody:    1 << 20,
		Rules:      []Rule{{Event: "push"}},
	}
	c := assert.NewAborting(t)

	for _, source := range []string{"github", "omada.ci_2", "a"} {
		r := base
		r.Notify = source
		c.NoError(r.validate(), "notify_source %q is valid", source)
	}
	for _, source := range []string{"", "GitHub", "GitHub Webhooks", "-x", strings.Repeat("a", 65)} {
		r := base
		r.Notify = source
		err := r.validate()
		c.Error(err, "notify_source %q must be refused", source)
		c.ErrorContains(err, "invalid source", "the error names the daemon's own rule")
	}
}

// TestConfigExampleParses loads the shipped example with its /run/secrets paths
// pointed at real files, so the example can never drift from the schema.
func TestConfigExampleParses(t *testing.T) {
	c := assert.NewAborting(t)
	raw, err := os.ReadFile("example.toml")
	c.NoError(err)

	dir := t.TempDir()
	replacements := map[string]string{
		"/run/secrets/rafiki-hooks-token":    "example-token",
		"/run/secrets/github-webhook-secret": "github-secret",
		"/run/secrets/omada-webhook-secret":  "omada-secret",
	}
	body := string(raw)
	for path, content := range replacements {
		local := filepath.Join(dir, filepath.Base(path))
		c.NoError(os.WriteFile(local, []byte(content), 0o600))
		body = strings.ReplaceAll(body, path, local)
	}

	f := newFixture(t)
	cfg, err := f.load(body)
	c.NoError(err)
	c.Eq("example-token", cfg.token)
	c.Len(cfg.Routes, 2)

	github, omada := cfg.Routes[0], cfg.Routes[1]
	c.Eq("/hooks/github", github.Path)
	c.Eq("github", github.Source)
	c.Eq("oncall", github.Target)
	c.Eq("github", github.Notify)
	c.Eq("github-secret", string(github.secret))
	c.Len(github.Rules, 1)
	c.Eq("workflow_run", github.Rules[0].Event)
	c.Eq("completed", github.Rules[0].Match["action"])
	c.Eq("failure", github.Rules[0].Match["workflow_run.conclusion"])
	c.Eq("main", github.Rules[0].Match["workflow_run.head_branch"])

	// The example has to FIRE on a real red run — that is the whole reason it
	// is shipped. GitHub's workflow_run puts the verdict in conclusion and
	// sends action = completed, so a rule matching action = "failure" matches
	// nothing GitHub ever sends, and an operator copying the example is silent.
	failed := payload(t, `{
		"action": "completed",
		"repository": {"full_name": "graveland/rafiki"},
		"workflow_run": {
			"id": 1234, "name": "ci", "head_branch": "main", "conclusion": "failure",
			"html_url": "https://github.com/graveland/rafiki/actions/runs/1234"
		}
	}`)
	c.True(github.Rules[0].matches("workflow_run", failed),
		"the example github rule must fire for a failed workflow_run on main")
	succeeded := payload(t, `{"action": "completed", "workflow_run": {"head_branch": "main", "conclusion": "success"}}`)
	c.False(github.Rules[0].matches("workflow_run", succeeded),
		"a successful run must not wake on-call")

	c.Eq("/hooks/omada", omada.Path)
	c.Eq("generic", omada.Source)
	c.Eq("X-Omada-Secret", omada.Header)
	c.Eq("network-ops", omada.Target)
	c.Eq("omada", omada.Notify)
	c.Eq("omada-secret", string(omada.secret))
	c.Len(omada.Rules, 1)
	c.Eq("", omada.Rules[0].Event)
	c.Eq("client_offline", omada.Rules[0].Match["event"])
}

// TestConfigUnknownKeyNamesEveryKey pins that the error lists every undecoded
// key, so one typo does not hide the next.
func TestConfigUnknownKeyNamesEveryKey(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	body := strings.Replace(f.valid(), `target = "oncall"`, "target = \"oncall\"\nmax_boddy = 1\nsecrett = 2", 1)
	_, err := f.load(body)
	c.Error(err)
	c.ErrorContains(err, "max_boddy")
	c.ErrorContains(err, "secrett")
}
