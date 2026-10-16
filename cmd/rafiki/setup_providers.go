// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/providers"
)

// noProviderNote is printed when the operator selects no provider at all.
const noProviderNote = "    note: no LLM provider — claude-kind agents run on your Claude login; fundi agents and recall summaries need a provider (an OpenRouter or Anthropic key, or a local Ollama — re-run rafiki setup to add one)"

// providersResult is what runSetup needs back from the providers step.
type providersResult struct {
	Changed     bool     // service.env or providers.toml was rewritten → restart
	Providers   []string // configured provider names, menu order then custom names sorted
	Default     string   // default_provider ("" when none)
	OllamaModel string   // chosen Ollama chat model ("" when none)
}

// providerMenu is the multi-select order. "other" is a single option standing
// for every custom OpenAI-compatible endpoint.
var providerMenu = []string{"anthropic", "openrouter", "ollama", "other"}

// customProviderNameRE mirrors providers' name rule, which is unexported there.
var customProviderNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// isBuiltinProvider reports whether name is one of the three menu providers.
func isBuiltinProvider(name string) bool {
	switch name {
	case "anthropic", "openrouter", "ollama":
		return true
	}
	return false
}

// runProvidersStep writes providers.toml and the provider keys in service.env,
// owning the file only while it is byte-identical to what setup last wrote. It
// is not wired into runSetup yet.
func runProvidersStep(ctx context.Context, w *wizard, r runner, out io.Writer, st *installstate.State, savedEnv map[string]string) (providersResult, error) {
	var res providersResult
	path := paths.ProvidersFile()

	state, cur, err := classifyProvidersFile(path, st.Providers.FileSHA256)
	if err != nil {
		return res, err
	}

	// ── a foreign file: someone edited it, or setup never wrote it.
	if state == providersFileForeign {
		action := "keep"
		if w.interactive {
			action, err = w.askSelect("providers.toml was edited outside setup", "",
				[]choice{
					{Value: "keep", Label: "keep it (validate only)"},
					{Value: "replace", Label: "replace it (the current file is backed up first)"},
				}, "", "keep")
			if err != nil {
				return res, err
			}
		}
		if action == "replace" {
			bp, err := backupProvidersFile(path, time.Now())
			if err != nil {
				return res, err
			}
			fmt.Fprintf(out, "    backed up to %s\n", bp)
			state = providersFileMissing
			cur = nil
		} else {
			return keepForeignProvidersFile(ctx, out, path, w, savedEnv)
		}
	}

	// ── the defaults a re-run starts from: the file setup owns, else nothing.
	var base *providers.Set
	if state == providersFileOwned {
		base, err = providers.Parse(cur)
		if err != nil {
			return res, fmt.Errorf("%s: %w", path, err)
		}
	}

	// ── the provider menu.
	chosen, err := w.askMulti(
		"LLM providers (other = any OpenAI-compatible endpoint: vmlx, LM Studio, vLLM)",
		"", providerMenu, menuSaved(base), menuDetected(w, st, savedEnv))
	if err != nil {
		return res, err
	}
	// A key flag always selects its provider, even when nothing else did.
	if w.cmd.Flags().Changed(keyAnthropicFlag) && !containsString(chosen, "anthropic") {
		chosen = append(chosen, "anthropic")
	}
	if w.cmd.Flags().Changed(keyOpenRouterFlag) && !containsString(chosen, "openrouter") {
		chosen = append(chosen, "openrouter")
	}

	set := &providers.Set{Providers: map[string]providers.Provider{}}
	vars := map[string]string{}
	ollamaModel := ""

	// ── per provider.
	if containsString(chosen, "anthropic") {
		key, err := resolveValidKey(ctx, w, out, "anthropic", keyAnthropicFlag, anthropicAPIKeyEnv, savedEnv)
		if err != nil {
			return res, err
		}
		if key == "" {
			fmt.Fprintln(out, "    anthropic: no key; not configured")
		} else {
			vars[anthropicAPIKeyEnv] = key
			set.Providers["anthropic"] = providers.Provider{Kind: providers.KindAnthropic, APIKeyEnv: anthropicAPIKeyEnv}
		}
	}
	if containsString(chosen, "openrouter") {
		key, err := resolveValidKey(ctx, w, out, "openrouter", keyOpenRouterFlag, openrouterAPIKeyEnv, savedEnv)
		if err != nil {
			return res, err
		}
		if key == "" {
			fmt.Fprintln(out, "    openrouter: no key; not configured")
		} else {
			vars[openrouterAPIKeyEnv] = key
			set.Providers["openrouter"] = providers.Provider{Kind: providers.KindAnthropicOpenRouter, APIKeyEnv: openrouterAPIKeyEnv}
		}
	}
	if containsString(chosen, "ollama") {
		ok, model, err := configureOllama(ctx, w, r, out, st)
		if err != nil {
			return res, err
		}
		if ok {
			set.Providers["ollama"] = providers.Provider{Kind: providers.KindOpenAI, BaseURL: ollamaBaseURL + "/v1"}
			ollamaModel = model
		}
	}
	if containsString(chosen, "other") {
		if err := configureCustomProviders(ctx, w, r, out, base, set, vars); err != nil {
			return res, err
		}
	}

	// ── zero providers: no file (an owned one moves to its backup), no default.
	if len(set.Providers) == 0 {
		fmt.Fprintln(out, noProviderNote)
		if state == providersFileOwned {
			bp, err := backupProvidersFile(path, time.Now())
			if err != nil {
				return res, err
			}
			if err := os.Remove(path); err != nil {
				return res, err
			}
			fmt.Fprintf(out, "    moved providers.toml to %s\n", bp)
			st.Providers.FileSHA256 = ""
			res.Changed = true
		}
		envChanged, err := writeEnvVars(vars)
		if err != nil {
			return res, err
		}
		if envChanged {
			res.Changed = true
		}
		return res, nil
	}

	ordered := orderedProviderNames(set)

	// ── default provider: only when more than one is configured.
	def := ordered[0]
	if len(ordered) > 1 {
		choices := make([]choice, 0, len(ordered))
		for _, n := range ordered {
			choices = append(choices, choice{Value: n, Label: n})
		}
		savedDef := ""
		if base != nil {
			savedDef = base.DefaultProvider
		}
		def, err = w.askSelect("Default provider", "", choices, savedDef, ordered[0])
		if err != nil {
			return res, err
		}
	}

	// ── fallback: anthropic falls back to openrouter when both are written,
	// mirroring providers.Default() so a keyed install loses nothing.
	if _, an := set.Providers["anthropic"]; an {
		if _, or := set.Providers["openrouter"]; or {
			p := set.Providers["anthropic"]
			p.Fallback = []string{"openrouter"}
			set.Providers["anthropic"] = p
		}
	}

	if err := chooseSummaries(w, set, base, ollamaModel); err != nil {
		return res, err
	}
	if err := chooseEmbeddings(ctx, w, r, out, set, base, vars); err != nil {
		return res, err
	}

	// ── write.
	set.DefaultProvider = def
	b, err := renderProviders(set)
	if err != nil {
		return res, err
	}
	changed, err := writeProvidersFile(path, b)
	if err != nil {
		return res, err
	}
	st.Providers.FileSHA256 = sha256Hex(b)
	envChanged, err := writeEnvVars(vars)
	if err != nil {
		return res, err
	}

	res.Changed = changed || envChanged
	res.Providers = ordered
	res.Default = def
	res.OllamaModel = ollamaModel
	return res, nil
}

// writeEnvVars writes vars to service.env and reports whether any key changed.
func writeEnvVars(vars map[string]string) (bool, error) {
	if len(vars) == 0 {
		return false, nil
	}
	envRes, err := paths.SetEnvFile(paths.ServiceEnvFile(), vars, "rafiki setup")
	if err != nil {
		return false, err
	}
	return len(envRes.Added) > 0 || len(envRes.Replaced) > 0, nil
}

// orderedProviderNames returns set's names in menu order (anthropic, openrouter,
// ollama) followed by the custom names sorted.
func orderedProviderNames(set *providers.Set) []string {
	ordered := make([]string, 0, len(set.Providers))
	for _, n := range []string{"anthropic", "openrouter", "ollama"} {
		if _, ok := set.Providers[n]; ok {
			ordered = append(ordered, n)
		}
	}
	var customs []string
	for n := range set.Providers {
		if !isBuiltinProvider(n) {
			customs = append(customs, n)
		}
	}
	sort.Strings(customs)
	return append(ordered, customs...)
}

// menuSaved maps an owned file's provider names onto the menu: a custom name
// becomes "other", so a re-run keeps it without naming it.
func menuSaved(base *providers.Set) []string {
	if base == nil {
		return nil
	}
	var out []string
	for _, n := range []string{"anthropic", "openrouter", "ollama"} {
		if _, ok := base.Providers[n]; ok {
			out = append(out, n)
		}
	}
	for n := range base.Providers {
		if !isBuiltinProvider(n) {
			out = append(out, "other")
			break
		}
	}
	return out
}

// menuDetected preselects the providers a flag or a saved key already implies,
// plus ollama when it is a chosen optional tool.
func menuDetected(w *wizard, st *installstate.State, savedEnv map[string]string) []string {
	var detected []string
	if w.cmd.Flags().Changed(keyAnthropicFlag) || savedEnv[anthropicAPIKeyEnv] != "" {
		detected = append(detected, "anthropic")
	}
	if w.cmd.Flags().Changed(keyOpenRouterFlag) || savedEnv[openrouterAPIKeyEnv] != "" {
		detected = append(detected, "openrouter")
	}
	if containsString(st.Optional, "ollama") {
		detected = append(detected, "ollama")
	}
	return detected
}

// keepForeignProvidersFile validates a file setup does not own, prints one line
// per provider, and prompts for (and stores) the key of every api_key_env it
// references. It never writes providers.toml.
func keepForeignProvidersFile(ctx context.Context, out io.Writer, path string, w *wizard, savedEnv map[string]string) (providersResult, error) {
	var res providersResult
	set, err := providers.Load(path)
	if err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	for _, name := range set.Names() {
		p := set.Providers[name]
		base := p.BaseURL
		if base == "" {
			base = "-"
		}
		key := p.APIKeyEnv
		if key == "" {
			key = "none"
		}
		fmt.Fprintf(out, "    %s: %s %s key=%s\n", name, p.Kind, base, key)
	}
	if set.Summaries != nil {
		fmt.Fprintf(out, "    summaries: %s\n", set.Summaries.Model)
	}
	if set.Embeddings != nil {
		fmt.Fprintf(out, "    embeddings: %s (%d dims)\n", set.Embeddings.Model, set.Embeddings.Dimensions)
	}

	vars := map[string]string{}
	for _, env := range referencedKeyEnvs(set) {
		var key string
		switch env {
		case anthropicAPIKeyEnv:
			key, err = resolveValidKey(ctx, w, out, "anthropic", keyAnthropicFlag, anthropicAPIKeyEnv, savedEnv)
		case openrouterAPIKeyEnv:
			key, err = resolveValidKey(ctx, w, out, "openrouter", keyOpenRouterFlag, openrouterAPIKeyEnv, savedEnv)
		default:
			key, err = w.askSecret(env+" (for providers.toml)", "", savedEnv[env], "", false)
		}
		if err != nil {
			return res, err
		}
		if key != "" {
			vars[env] = key
		}
	}
	envChanged, err := writeEnvVars(vars)
	if err != nil {
		return res, err
	}
	res.Changed = envChanged
	res.Providers = set.Names()
	res.Default = set.DefaultProvider
	return res, nil
}

// referencedKeyEnvs returns every distinct non-empty api_key_env the file
// references, sorted for a deterministic prompt order.
func referencedKeyEnvs(set *providers.Set) []string {
	seen := map[string]bool{}
	var out []string
	add := func(env string) {
		if env != "" && !seen[env] {
			seen[env] = true
			out = append(out, env)
		}
	}
	for _, name := range set.Names() {
		add(set.Providers[name].APIKeyEnv)
	}
	if set.Embeddings != nil {
		add(set.Embeddings.APIKeyEnv)
	}
	sort.Strings(out)
	return out
}

// configureOllama makes the local Ollama usable — installing it when missing
// and the operator agrees, waiting for it to answer, and choosing a chat model.
// It returns whether ollama is configured and the chosen chat model ("" when
// none). Under --non-interactive it never installs, pulls, or waits: a missing
// or silent Ollama is dropped.
func configureOllama(ctx context.Context, w *wizard, r runner, out io.Writer, st *installstate.State) (bool, string, error) {
	if _, err := r.LookPath("ollama"); err != nil {
		if !w.interactive {
			fmt.Fprintln(out, "    ollama: not installed; skipped")
			return false, "", nil
		}
		install := ollamaInstaller()
		if install == nil {
			fmt.Fprintln(out, "    ollama: no installer for this platform; skipped")
			return false, "", nil
		}
		hasBrew := false
		if _, err := r.LookPath("brew"); err == nil {
			hasBrew = true
		}
		for _, argv := range install(runtime.GOOS, hasBrew) {
			if err := runInstallCommand(ctx, r, w, "ollama", argv, runtime.GOOS, hasBrew); err != nil {
				return false, "", err
			}
		}
		if !containsString(st.Optional, "ollama") {
			st.Optional = append(st.Optional, "ollama")
		}
	}

	answering := ollamaAnswering(ctx)
	if !answering {
		fmt.Fprintf(out, "    ollama is not answering at %s; start it with `ollama serve` or the Ollama app\n", ollamaBaseURL)
		if !w.interactive {
			fmt.Fprintln(out, "    ollama: not answering; skipped")
			return false, "", nil
		}
	loop:
		for {
			action, err := w.askSelect("Ollama", "", []choice{
				{Value: "retry", Label: "retry"},
				{Value: "skip", Label: "skip"},
				{Value: "keep", Label: "keep it anyway (unvalidated)"},
			}, "", "retry")
			if err != nil {
				return false, "", err
			}
			switch action {
			case "skip":
				return false, "", nil
			case "keep":
				break loop
			}
			if ollamaAnswering(ctx) {
				answering = true
				break loop
			}
		}
	}

	model := ""
	if w.interactive && answering {
		models, err := ollamaModels(ctx, r)
		if err != nil {
			return false, "", err
		}
		choices := make([]choice, 0, len(models)+3)
		for _, m := range models {
			choices = append(choices, choice{Value: m, Label: m})
		}
		choices = append(choices,
			choice{Value: "pull", Label: "pull " + ollamaSuggestedChatModel},
			choice{Value: "type", Label: "type a model name"},
			choice{Value: "skip", Label: "skip"})
		detected := "pull"
		if len(models) > 0 {
			detected = models[0]
		}
		sel, err := w.askSelect("Ollama model", "", choices, "", detected)
		if err != nil {
			return false, "", err
		}
		switch sel {
		case "skip":
			// No chat model; the provider is still usable by explicit model id.
		case "pull":
			if err := ollamaPull(ctx, r, ollamaSuggestedChatModel); err != nil {
				return false, "", err
			}
			model = ollamaSuggestedChatModel
		case "type":
			name, err := w.askString("Ollama model name", "", "", "", true)
			if err != nil {
				return false, "", err
			}
			if !containsString(models, name) {
				if err := ollamaPull(ctx, r, name); err != nil {
					return false, "", err
				}
			}
			model = name
		default:
			model = sel
		}
	}
	return true, model, nil
}

// ollamaInstaller finds the ollama prereq's Install function, or nil when the
// list has no ollama entry.
func ollamaInstaller() func(goos string, hasBrew bool) [][]string {
	for _, p := range prereqs() {
		if p.Name == "ollama" {
			return p.Install
		}
	}
	return nil
}

// configureCustomProviders copies a base file's custom endpoints and — when
// interactive — offers to keep each and to add more. It writes providers into
// set and any key values into vars.
func configureCustomProviders(ctx context.Context, w *wizard, r runner, out io.Writer, base *providers.Set, set *providers.Set, vars map[string]string) error {
	kept := 0
	if base != nil {
		for _, name := range base.Names() {
			if isBuiltinProvider(name) {
				continue
			}
			p := base.Providers[name]
			if w.interactive {
				keep, err := w.askConfirm(fmt.Sprintf("Keep endpoint %s (%s)?", name, p.BaseURL), "", nil, true)
				if err != nil {
					return err
				}
				if !keep {
					continue
				}
			}
			set.Providers[name] = p
			kept++
		}
	}
	if !w.interactive {
		return nil
	}

	for {
		add, err := w.askConfirm("Add an OpenAI-compatible endpoint?", "", nil, kept == 0)
		if err != nil {
			return err
		}
		if !add {
			return nil
		}
		name, err := askProviderName(w, out, set)
		if err != nil {
			return err
		}
		baseURL, err := askProviderBaseURL(w, out)
		if err != nil {
			return err
		}
		key, err := w.askSecret("API key (optional)", "", "", "", false)
		if err != nil {
			return err
		}
		env := ""
		if key != "" {
			env = customProviderKeyEnv(name)
		}
		keep, err := validateEndpointWithRetry(ctx, w, out, baseURL, key)
		if err != nil {
			return err
		}
		if !keep {
			// Do not store the key for an endpoint the operator skipped: an
			// orphan service.env entry outlives the provider it belonged to.
			continue
		}
		if key != "" {
			vars[env] = key
		}
		set.Providers[name] = providers.Provider{Kind: providers.KindOpenAI, BaseURL: baseURL, APIKeyEnv: env}
		kept++
	}
}

// customProviderKeyEnv is the service.env name a custom endpoint's key is
// stored under.
func customProviderKeyEnv(name string) string {
	return "RAFIKI_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_API_KEY"
}

// askProviderName prompts for a custom endpoint name, re-asking interactively
// until it is a legal, unused name.
func askProviderName(w *wizard, out io.Writer, set *providers.Set) (string, error) {
	for {
		name, err := w.askString("Endpoint name", "", "", "", true)
		if err != nil {
			return "", err
		}
		if !customProviderNameRE.MatchString(name) {
			fmt.Fprintf(out, "    %q is not a valid name (want [a-z0-9][a-z0-9_-]*)\n", name)
			if !w.interactive {
				return "", fmt.Errorf("invalid endpoint name %q", name)
			}
			continue
		}
		if isBuiltinProvider(name) {
			fmt.Fprintf(out, "    %q is reserved\n", name)
			if !w.interactive {
				return "", fmt.Errorf("endpoint name %q is reserved", name)
			}
			continue
		}
		if _, exists := set.Providers[name]; exists {
			fmt.Fprintf(out, "    %q is already used\n", name)
			if !w.interactive {
				return "", fmt.Errorf("endpoint name %q is already used", name)
			}
			continue
		}
		return name, nil
	}
}

// askProviderBaseURL prompts for a custom endpoint's base URL, re-asking
// interactively until it is an absolute http(s) URL.
func askProviderBaseURL(w *wizard, out io.Writer) (string, error) {
	for {
		raw, err := w.askString("Base URL (…/v1)", "", "", "", true)
		if err != nil {
			return "", err
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fmt.Fprintf(out, "    %q is not an absolute http(s) URL\n", raw)
			if !w.interactive {
				return "", fmt.Errorf("invalid base URL %q", raw)
			}
			continue
		}
		return raw, nil
	}
}

// validateEndpointWithRetry checks a custom endpoint with GET /models. On
// failure it prints the error and offers retry/skip/keep. It returns keep=false
// only when the operator skips the endpoint.
func validateEndpointWithRetry(ctx context.Context, w *wizard, out io.Writer, baseURL, key string) (bool, error) {
	if err := checkOpenAIModels(ctx, baseURL, key); err == nil {
		return true, nil
	} else {
		fmt.Fprintf(out, "    endpoint %s: %v\n", baseURL, err)
	}
	if !w.interactive {
		return false, fmt.Errorf("endpoint %s did not answer GET /models", baseURL)
	}
	for {
		action, err := w.askSelect("Endpoint "+baseURL, "", []choice{
			{Value: "retry", Label: "retry"},
			{Value: "skip", Label: "skip"},
			{Value: "keep", Label: "keep it anyway (unvalidated)"},
		}, "", "retry")
		if err != nil {
			return false, err
		}
		switch action {
		case "skip":
			return false, nil
		case "keep":
			return true, nil
		}
		if err := checkOpenAIModels(ctx, baseURL, key); err == nil {
			return true, nil
		}
	}
}

// chooseSummaries asks for the conversation-summaries model and stores it on
// set. Under --non-interactive the owned file's model is kept when it is one of
// the offered choices, else summaries are skipped.
func chooseSummaries(w *wizard, set *providers.Set, base *providers.Set, ollamaModel string) error {
	var savedCfg *providers.SummariesConfig
	saved := ""
	if base != nil && base.Summaries != nil {
		savedCfg = base.Summaries
		saved = base.Summaries.Model
	}

	// A non-interactive re-run must keep what setup already wrote: copy the
	// owned [summaries] verbatim rather than re-deriving it from a choice list
	// that may not contain the model (a custom id, or ollama/<m> whose chat
	// model is not re-chosen non-interactively — ollamaModel is "" then). A
	// re-derivation there would silently drop [summaries] and rewrite the file.
	if !w.interactive {
		if savedCfg != nil {
			c := *savedCfg
			set.Summaries = &c
		}
		return nil
	}

	choices := []choice{{Value: "skip", Label: "skip — no conversation summaries"}}
	if _, ok := set.Providers["anthropic"]; ok {
		choices = append(choices, choice{Value: "anthropic/claude-haiku-4-5", Label: "anthropic/claude-haiku-4-5"})
	}
	if _, ok := set.Providers["openrouter"]; ok {
		choices = append(choices, choice{Value: "openrouter/z-ai/glm-5.3-flash", Label: "openrouter/z-ai/glm-5.3-flash"})
	}
	if ollamaModel != "" {
		choices = append(choices, choice{Value: "ollama/" + ollamaModel, Label: "ollama/" + ollamaModel})
	}
	// Keep the owned model reachable even when it is none of the suggestions,
	// so a bare Enter preserves it instead of silently dropping it.
	if saved != "" && !choiceHas(choices, saved) {
		choices = append(choices, choice{Value: saved, Label: "keep the current: " + saved})
	}
	choices = append(choices, choice{Value: "type", Label: "type a provider/model id"})

	m, err := w.askSelect("Model for conversation summaries (runs in the background; costs tokens on cloud providers)", "", choices, saved, "skip")
	if err != nil {
		return err
	}
	if m == "type" {
		m, err = w.askString("Summaries model (provider/model)", "", saved, "", true)
		if err != nil {
			return err
		}
	}
	if m == "skip" || m == "" {
		return nil
	}
	cfg := &providers.SummariesConfig{Model: m}
	if savedCfg != nil && savedCfg.Model == m {
		cfg.MaxSegmentTokens = savedCfg.MaxSegmentTokens
	}
	set.Summaries = cfg
	return nil
}

// embeddingsPick is a resolved embeddings endpoint before it is probed: keyValue
// is the credential to send, never written to providers.toml.
type embeddingsPick struct {
	URL       string
	APIKeyEnv string
	KeyValue  string
	Model     string
}

// chooseEmbeddings offers embeddings when an embeddings-capable provider is
// configured, probes the endpoint, and stores it on set. Under
// --non-interactive the owned file's embeddings are copied unprobed.
func chooseEmbeddings(ctx context.Context, w *wizard, r runner, out io.Writer, set *providers.Set, base *providers.Set, vars map[string]string) error {
	if !embeddingsEligible(set) {
		return nil
	}
	if !w.interactive {
		if base != nil && base.Embeddings != nil {
			e := *base.Embeddings
			set.Embeddings = &e
		}
		return nil
	}

	choices := []choice{{Value: "skip", Label: "skip — keyword-only recall"}}
	if _, ok := set.Providers["openrouter"]; ok {
		choices = append(choices, choice{Value: "openrouter", Label: "openrouter: " + openrouterSuggestedEmbedModel})
	}
	if _, ok := set.Providers["ollama"]; ok {
		choices = append(choices, choice{Value: "ollama", Label: "ollama: " + ollamaSuggestedEmbedModel})
	}
	for _, name := range customProviderNames(set) {
		choices = append(choices, choice{Value: "custom:" + name, Label: name + ": type a model"})
	}
	if base != nil && base.Embeddings != nil {
		choices = append(choices, choice{Value: "keep", Label: "keep the current: " + base.Embeddings.Model})
	}

	for {
		sel, err := w.askSelect("Embeddings (vector recall)", "", choices, "", "skip")
		if err != nil {
			return err
		}
		switch sel {
		case "skip":
			return nil
		case "keep":
			e := *base.Embeddings
			set.Embeddings = &e
			return nil
		}
		pick, err := resolveEmbeddingsChoice(ctx, w, r, set, vars, sel)
		if err != nil {
			return err
		}
		dims, err := probeEmbeddings(ctx, pick.URL, pick.KeyValue, pick.Model)
		if err != nil {
			fmt.Fprintf(out, "    embeddings: %v\n", err)
			continue
		}
		set.Embeddings = &providers.EmbeddingsConfig{
			URL:        pick.URL,
			APIKeyEnv:  pick.APIKeyEnv,
			Model:      pick.Model,
			Dimensions: dims,
		}
		return nil
	}
}

// resolveEmbeddingsChoice resolves one embeddings selection to a concrete
// endpoint, pulling an Ollama embedding model when it is missing.
func resolveEmbeddingsChoice(ctx context.Context, w *wizard, r runner, set *providers.Set, vars map[string]string, sel string) (embeddingsPick, error) {
	if sel == "openrouter" {
		return embeddingsPick{
			URL:       openrouterEmbeddingsURL,
			APIKeyEnv: openrouterAPIKeyEnv,
			KeyValue:  vars[openrouterAPIKeyEnv],
			Model:     openrouterSuggestedEmbedModel,
		}, nil
	}
	if sel == "ollama" {
		models, err := ollamaModels(ctx, r)
		if err != nil {
			return embeddingsPick{}, err
		}
		if !containsString(models, ollamaSuggestedEmbedModel) {
			if err := ollamaPull(ctx, r, ollamaSuggestedEmbedModel); err != nil {
				return embeddingsPick{}, err
			}
		}
		return embeddingsPick{
			URL:   ollamaBaseURL + "/v1/embeddings",
			Model: ollamaSuggestedEmbedModel,
		}, nil
	}
	name := strings.TrimPrefix(sel, "custom:")
	p := set.Providers[name]
	model, err := w.askString("Embeddings model (provider/model)", "", "", "", true)
	if err != nil {
		return embeddingsPick{}, err
	}
	return embeddingsPick{
		URL:       strings.TrimRight(p.BaseURL, "/") + "/embeddings",
		APIKeyEnv: p.APIKeyEnv,
		KeyValue:  vars[p.APIKeyEnv],
		Model:     model,
	}, nil
}

// embeddingsEligible reports whether a configured provider can back embeddings:
// openrouter, ollama, or a custom OpenAI-compatible endpoint.
func embeddingsEligible(set *providers.Set) bool {
	if _, ok := set.Providers["openrouter"]; ok {
		return true
	}
	if _, ok := set.Providers["ollama"]; ok {
		return true
	}
	return len(customProviderNames(set)) > 0
}

// customProviderNames returns set's custom (non-builtin) provider names, sorted.
func customProviderNames(set *providers.Set) []string {
	var out []string
	for n := range set.Providers {
		if !isBuiltinProvider(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
