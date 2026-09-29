// SPDX-License-Identifier: Apache-2.0

package oidclogin

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestLoadConfigMissingFile pins the (nil, nil) contract: no oidc.toml is the
// daemon's ordinary state, not an error.
func TestLoadConfigMissingFile(t *testing.T) {
	c := assert.NewAborting(t)
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "oidc.toml"))
	c.NoError(err, "LoadConfig on a missing file")
	c.Nil(cfg, "cfg on a missing file")
}

// TestLoadConfigValidation walks the load-time refusals — every one is a
// config error the daemon logs and then runs without login — plus the valid
// file's defaults.
func TestLoadConfigValidation(t *testing.T) {
	t.Setenv("RAFIKI_TEST_OIDC_SECRET", "sekrit")
	secret := "client_secret_env = \"RAFIKI_TEST_OIDC_SECRET\"\n"

	dir := t.TempDir()
	counter := 0
	write := func(content string) string {
		counter++
		path := filepath.Join(dir, fmt.Sprintf("oidc-%d.toml", counter))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}

	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "missing issuer",
			content: "client_id = \"c\"\n" + secret + "email_domains = [\"graveland.dev\"]\n",
			wantErr: "issuer",
		},
		{
			name:    "plain-http issuer",
			content: "issuer = \"http://example.com\"\nclient_id = \"c\"\n" + secret + "email_domains = [\"graveland.dev\"]\n",
			wantErr: "issuer",
		},
		{
			name:    "missing client_id",
			content: "issuer = \"https://idp.example.com\"\n" + secret + "email_domains = [\"graveland.dev\"]\n",
			wantErr: "client_id",
		},
		{
			name:    "missing client_secret_env",
			content: "issuer = \"https://idp.example.com\"\nclient_id = \"c\"\nemail_domains = [\"graveland.dev\"]\n",
			wantErr: "client_secret_env",
		},
		{
			name:    "named secret env is empty",
			content: "issuer = \"https://idp.example.com\"\nclient_id = \"c\"\nclient_secret_env = \"RAFIKI_TEST_OIDC_ABSENT\"\nemail_domains = [\"graveland.dev\"]\n",
			wantErr: "environment variable is empty",
		},
		{
			name:    "no email domains",
			content: "issuer = \"https://idp.example.com\"\nclient_id = \"c\"\n" + secret,
			wantErr: "email_domains",
		},
		{
			name:    "email domain with an @",
			content: "issuer = \"https://idp.example.com\"\nclient_id = \"c\"\n" + secret + "email_domains = [\"alice@graveland.dev\"]\n",
			wantErr: "no @",
		},
		{
			name:    "negative session_ttl",
			content: "issuer = \"https://idp.example.com\"\nclient_id = \"c\"\n" + secret + "email_domains = [\"graveland.dev\"]\nsession_ttl = \"-5m\"\n",
			wantErr: "session_ttl",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			cfg, err := LoadConfig(write(tc.content))
			c.Error(err, "LoadConfig must refuse an invalid file")
			c.Nil(cfg, "cfg on an invalid file")
			c.StrContains(err.Error(), tc.wantErr, "error text")
		})
	}

	t.Run("valid file applies defaults", func(t *testing.T) {
		c := assert.NewAborting(t)
		cfg, err := LoadConfig(write(
			"issuer = \"https://idp.example.com\"\nclient_id = \"c\"\n" + secret +
				"email_domains = [\"Graveland.Dev\"]\n"))
		c.NoError(err, "LoadConfig on a valid file")
		if cfg == nil {
			t.Fatal("cfg is nil")
		}
		c.Eq("https://idp.example.com", cfg.Issuer, "issuer")
		c.Eq("c", cfg.ClientID, "client_id")
		c.Eq(Duration(12*time.Hour), cfg.SessionTTL, "session_ttl default")
		c.ElementsMatch([]string{"openid", "email", "profile"}, cfg.Scopes, "scopes default")
		c.ElementsMatch([]string{"graveland.dev"}, cfg.EmailDomains, "domains lowercased")
	})

	t.Run("loopback http issuer accepted", func(t *testing.T) {
		c := assert.NewAborting(t)
		cfg, err := LoadConfig(write(
			"issuer = \"http://127.0.0.1:9999\"\nclient_id = \"c\"\n" + secret +
				"email_domains = [\"graveland.dev\"]\n"))
		c.NoError(err, "LoadConfig on a loopback http issuer")
		if cfg == nil {
			t.Fatal("cfg is nil")
		}
		c.Eq("http://127.0.0.1:9999", cfg.Issuer, "issuer")
	})

	t.Run("localhost http issuer accepted", func(t *testing.T) {
		c := assert.NewAborting(t)
		cfg, err := LoadConfig(write(
			"issuer = \"http://localhost:9999\"\nclient_id = \"c\"\n" + secret +
				"email_domains = [\"graveland.dev\"]\n"))
		c.NoError(err, "LoadConfig on a localhost http issuer")
		if cfg == nil {
			t.Fatal("cfg is nil")
		}
		c.Eq("http://localhost:9999", cfg.Issuer, "issuer")
	})
}
