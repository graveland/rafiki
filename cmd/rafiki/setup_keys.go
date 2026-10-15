// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
)

// The setup flags that carry a provider key.
const (
	keyOpenRouterFlag = "openrouter-key"
	keyAnthropicFlag  = "anthropic-key"
)

// keyCheckBaseURLs maps a provider name to the base URL of the endpoint whose
// GET answers whether a key is valid. A package var so tests can point it at an
// httptest.Server.
var keyCheckBaseURLs = map[string]string{
	"openrouter": "https://openrouter.ai",
	"anthropic":  "https://api.anthropic.com",
}

// keyTitles are the prompt titles for each provider.
var keyTitles = map[string]string{
	"openrouter": "OpenRouter API key (optional)",
	"anthropic":  "Anthropic API key (optional)",
}

// runKeysStep resolves and validates the two provider keys, writing the valid
// ones to service.env. It returns (a) whether service.env changed and (b)
// whether any key is set (so the final line can offer a fundi model). With
// neither key set it prints the exact note.
func runKeysStep(ctx context.Context, w *wizard, out io.Writer, savedEnv map[string]string) (changed, haveKey bool, err error) {
	orKey, err := resolveValidKey(ctx, w, out, "openrouter", keyOpenRouterFlag, openrouterAPIKeyEnv, savedEnv)
	if err != nil {
		return false, false, err
	}
	anKey, err := resolveValidKey(ctx, w, out, "anthropic", keyAnthropicFlag, anthropicAPIKeyEnv, savedEnv)
	if err != nil {
		return false, false, err
	}

	vars := map[string]string{}
	if orKey != "" {
		vars[openrouterAPIKeyEnv] = orKey
	}
	if anKey != "" {
		vars[anthropicAPIKeyEnv] = anKey
	}
	if len(vars) > 0 {
		res, err := paths.SetEnvFile(paths.ServiceEnvFile(), vars, "rafiki setup")
		if err != nil {
			return false, false, err
		}
		if len(res.Added) > 0 || len(res.Replaced) > 0 {
			changed = true
		}
	}
	if orKey == "" && anKey == "" {
		fmt.Fprintln(out, noKeyNote)
	}
	return changed, orKey != "" || anKey != "", nil
}

// resolveValidKey asks for one provider's key and, when a non-empty answer is
// given, validates it with a live call. An invalid key re-prompts
// interactively; under --yes (or a piped stdin) it is an error. An empty answer
// (no key configured) is not an error.
func resolveValidKey(ctx context.Context, w *wizard, out io.Writer, provider, flag, envName string, savedEnv map[string]string) (string, error) {
	saved := savedEnv[envName]
	for {
		key, err := w.askSecret(keyTitles[provider], flag, saved, "", false)
		if err != nil {
			return "", err
		}
		if key == "" {
			return "", nil
		}
		if verr := validateKey(ctx, provider, key); verr != nil {
			if !w.interactive {
				return "", fmt.Errorf("%s API key from --%s is invalid: %w", provider, flag, verr)
			}
			fmt.Fprintf(out, "    %s API key was rejected (%v); try again\n", provider, verr)
			saved = "" // do not re-offer a value already known bad
			continue
		}
		return key, nil
	}
}

// validateKey makes one live check against the provider. A 200 means the key is
// valid; anything else (including a transport error) is a failure. It never
// sets Accept-Encoding by hand — doing so would disable Go's transparent
// gzip handling in http.Transport.
func validateKey(ctx context.Context, provider, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	base := keyCheckBaseURLs[provider]
	var (
		url string
		req *http.Request
		err error
	)
	switch provider {
	case "openrouter":
		url = base + "/api/v1/key"
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
	case "anthropic":
		url = base + "/v1/models"
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		return fmt.Errorf("unknown key provider %q", provider)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
