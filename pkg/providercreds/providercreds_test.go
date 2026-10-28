// SPDX-License-Identifier: Apache-2.0

package providercreds_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/providercreds"

	"github.com/multigres/testkit/assert"
)

// A Credential holds two live bearer tokens, so no formatting verb and no log
// handler may print them. String covers %v/%s/%+v, GoString covers %#v (fmt
// consults GoStringer there, never Stringer), and LogValue covers slog.
func TestProviderCredentialRedactsTokens(t *testing.T) {
	c := assert.NewAborting(t)
	cred := providercreds.Credential{
		OwnerUserID:  "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
		Provider:     "openai-responses-chatgpt",
		AccountID:    "acct_redaction",
		RefreshToken: "refresh-token-must-not-print",
		AccessToken:  "access-token-must-not-print",
		AccessExpiry: time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC),
		ModelsJSON:   []byte(`[{"id":"gpt-5"}]`),
		ModelsETag:   `W/"etag"`,
		LastRefresh:  time.Date(2026, 5, 4, 2, 0, 0, 0, time.UTC),
	}

	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		out := fmt.Sprintf(format, cred)
		c.NotStrContains(out, cred.RefreshToken, "%s leaked the refresh token: %s", format, out)
		c.NotStrContains(out, cred.AccessToken, "%s leaked the access token: %s", format, out)
		c.StrContains(out, "<redacted>", "%s does not mark the tokens redacted: %s", format, out)
		c.StrContains(out, cred.Provider, "%s dropped the non-secret fields: %s", format, out)
	}

	// The same rule through a structured logger, which is the other place a
	// whole struct tends to end up.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("login", "credential", cred)
	c.NotStrContains(buf.String(), cred.RefreshToken, "slog leaked the refresh token: %s", buf.String())
	c.NotStrContains(buf.String(), cred.AccessToken, "slog leaked the access token: %s", buf.String())
	c.StrContains(buf.String(), cred.Provider, "slog dropped the non-secret fields: %s", buf.String())
	c.StrContains(buf.String(), "<redacted>", "slog does not mark the tokens redacted: %s", buf.String())
}
