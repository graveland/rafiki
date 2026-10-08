// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/table"
)

// newSandboxCmd is the `rafiki sandbox` noun: create a named sandbox, list
// them, and remove one. Everything goes through the daemon's Control service
// (newConnectEndpoint) — the client never dials a sandbox or a launcher
// directly. A named sandbox is reached by name or id; a spawn-block sandbox
// belongs to a child and is not managed here.
func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Create and manage sandboxes",
		Long: `Create and manage sandboxes.

  create     create a named sandbox and print it
  ls         list sandboxes
  rm         remove a sandbox by name or id
  sync       copy a tree between two executors
  sync-repo  move a git branch between two executors

A named sandbox is a container running an executor, chosen with its own
mounts, network and resource limits, reached directly by name or id. A
sandbox bound to a child as a spawn block is not managed by this command.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newSandboxCreateCmd(),
		newSandboxListCmd(),
		newSandboxRemoveCmd(),
		newSandboxSyncCmd(),
		newSandboxSyncRepoCmd(),
	)
	return cmd
}

// sandboxMountKinds are the mount kinds the --mount flag accepts, in the order
// its help text names them. Shared by the parser, the flag's help and its
// completion so the three cannot drift.
var sandboxMountKinds = []string{
	string(protocol.MountRO),
	string(protocol.MountRW),
	string(protocol.MountEphemeral),
}

// sandboxNetworkValues are the closed set --network accepts. Empty is left off
// the list deliberately: it means "the daemon's configured default", which is
// not a value a user should be offered as a completion.
var sandboxNetworkValues = []string{
	string(protocol.NetworkEgress),
	string(protocol.NetworkNone),
}

// bindSandboxCompletions registers the closed-value completions for create's
// --network and --mount flags. A completion handler must never exit or block,
// so both are pure prefix filters over fixed lists.
func bindSandboxCompletions(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("network", cobra.FixedCompletions(
		sandboxNetworkValues, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("mount", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeSandboxMountKinds(toComplete), cobra.ShellCompDirectiveNoFileComp
	})
}

// completeSandboxMountKinds offers the mount kinds as "<kind>:" prefixes for
// the repeatable --mount flag, so a shell completes the front of a mount spec
// ("ro:", "rw:", "ephemeral:") and the user appends the target.
func completeSandboxMountKinds(toComplete string) []string {
	var out []string
	for _, k := range sandboxMountKinds {
		cand := k + ":"
		if strings.HasPrefix(cand, toComplete) {
			out = append(out, cand)
		}
	}
	return out
}

// ─── create ────────────────────────────────────────────────────────────────────

func newSandboxCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a named sandbox",
		Long: `Create a named sandbox and print it.

--name is required: a named sandbox is addressed by name (or id) afterwards.
--image may be omitted to take the daemon's RAFIKI_SANDBOX_IMAGE (default: the published ghcr sandbox image). --ttl may be
omitted to take the daemon's configured default (RAFIKI_SANDBOX_TTL); --network
may be omitted to take RAFIKI_SANDBOX_NETWORK.

A --mount value is <kind>:<target>, optionally with a source:

  ro:/work=host:/srv/repos/app   read-only bind of a host path
  rw:/scratch=volume:scratch     read-write bind of a named volume
  rw:/scratch                    read-write anonymous volume (removed with it)
  ephemeral:/tmp                 tmpfs (no source)

kind is ro, rw or ephemeral. A host path must lie under a launcher's
--sandbox-mount-root; the daemon refuses otherwise.`,
		RunE: runSandboxCreate,
	}
	cmd.Flags().String("name", "", "Sandbox name (required; what `rm` and completion accept)")
	cmd.Flags().String("image", "", "Container image (default: the daemon's RAFIKI_SANDBOX_IMAGE)")
	cmd.Flags().String("launcher", "", "Executor ref/selector to launch on; empty uses the sole docker launcher in scope")
	cmd.Flags().String("workdir", "", "Absolute working directory inside the container")
	cmd.Flags().String("network", "", "Network reach: egress|none (default: the daemon's configured default)")
	cmd.Flags().Bool("read-only-rootfs", false, "Mount the container's root filesystem read-only")
	cmd.Flags().String("user", "", "User to run as inside the container (uid[:gid])")
	cmd.Flags().String("memory", "", "Memory limit in bytes, or with a suffix (e.g. 512m, 1g)")
	cmd.Flags().Float64("cpus", 0, "CPU limit in fractional cores (e.g. 1.5)")
	cmd.Flags().Int64("pids", 0, "Maximum number of processes")
	cmd.Flags().Duration("ttl", 0, "Lifetime of the sandbox (e.g. 24h; default: the daemon's configured default)")
	cmd.Flags().StringArray("env", nil, "Environment variable in the container (repeatable, K=V)")
	cmd.Flags().StringArray("label", nil, "Label on the sandbox row (repeatable, K=V)")
	cmd.Flags().StringArray("mount", nil, "Mount, <kind>:<target>[=host:<path>|volume:<name>] (repeatable)")
	bindSandboxCompletions(cmd)
	return cmd
}

func runSandboxCreate(cmd *cobra.Command, _ []string) error {
	// The output mode is a user-input decision (-j and -J together is an
	// error), so resolve it before any round trip.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		return fmt.Errorf("--name is required for a named sandbox")
	}
	image, _ := cmd.Flags().GetString("image")
	launcher, _ := cmd.Flags().GetString("launcher")
	workdir, _ := cmd.Flags().GetString("workdir")
	network, _ := cmd.Flags().GetString("network")
	readOnlyRootfs, _ := cmd.Flags().GetBool("read-only-rootfs")
	user, _ := cmd.Flags().GetString("user")
	cpus, _ := cmd.Flags().GetFloat64("cpus")
	pids, _ := cmd.Flags().GetInt64("pids")

	var memBytes int64
	if memFlag, _ := cmd.Flags().GetString("memory"); memFlag != "" {
		memBytes, err = parseSandboxMemoryBytes(memFlag)
		if err != nil {
			return err
		}
	}

	mountFlags, _ := cmd.Flags().GetStringArray("mount")
	mounts, err := parseSandboxMounts(mountFlags)
	if err != nil {
		return err
	}

	envPairs, _ := cmd.Flags().GetStringArray("env")
	env, err := parseEnvPairs(envPairs)
	if err != nil {
		return fmt.Errorf("--env: %w", err)
	}
	labelPairs, _ := cmd.Flags().GetStringArray("label")
	labels, err := parseLabelPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("--label: %w", err)
	}

	spec := &rafikiv1.SandboxSpec{
		Name:           name,
		Launcher:       launcher,
		Image:          image,
		Mounts:         mounts,
		Workdir:        workdir,
		Network:        network,
		ReadOnlyRootfs: readOnlyRootfs,
		Env:            env,
		User:           user,
		MemoryBytes:    memBytes,
		Cpus:           cpus,
		PidsLimit:      pids,
		Labels:         labels,
	}
	// An unset --ttl means "the daemon's configured default", which is not the
	// same as an explicit 0: leave the field unset rather than sending zero.
	if cmd.Flags().Changed("ttl") {
		ttl, _ := cmd.Flags().GetDuration("ttl")
		spec.Ttl = durationpb.New(ttl)
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().CreateSandbox(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.CreateSandboxRequest{Spec: spec}))
	if err != nil {
		return connectVerbErr(err, ep.describe)
	}
	// The echo is the response's canonical protojson in every mode — this verb
	// renders no table, exactly like `executor create`.
	return emitProto(os.Stdout, resp.Msg, mode)
}

// ─── ls ──────────────────────────────────────────────────────────────────────

func newSandboxListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sandboxes",
		Long: `List sandboxes.

Columns: NAME, ID, STATE, NETWORK, IMAGE, LAUNCHER, EXPIRES, CONNECTED.
EXPIRES is the row's expiry time ("-" when it does not expire); CONNECTED is
whether the sandbox's executor is talking to the daemon right now.`,
		RunE: runSandboxList,
	}
	return cmd
}

func runSandboxList(cmd *cobra.Command, _ []string) error {
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().ListSandboxes(cmdCtx(cmd), connect.NewRequest(&rafikiv1.ListSandboxesRequest{}))
	if err != nil {
		return connectVerbErr(err, ep.describe)
	}
	rows := resp.Msg.GetSandboxes()

	if mode == outputTable {
		return renderSandboxTable(os.Stdout, rows, useColor)
	}
	// JSON is the canonical protojson of the wire's SandboxInfo rows: the
	// {"rows":[...]} envelope in -o json, one compact row per line under -J.
	return emitProtoRows(os.Stdout, rows, mode)
}

// renderSandboxTable writes the sandbox list as a table. When useColor is
// false no styles are applied, so piped output carries no ANSI escapes.
func renderSandboxTable(w io.Writer, sandboxes []*rafikiv1.SandboxInfo, useColor bool) error {
	if len(sandboxes) == 0 {
		fmt.Fprintln(w, "No sandboxes.")
		return nil
	}

	rows := make([][]string, len(sandboxes))
	for i, sb := range sandboxes {
		connected := "no"
		if sb.GetConnected() {
			connected = "yes"
		}
		rows[i] = []string{
			defaultDash(sb.GetName()),
			sb.GetId(),
			defaultDash(sb.GetState()),
			defaultDash(sb.GetNetwork()),
			defaultDash(sb.GetImage()),
			defaultDash(sb.GetLauncher()),
			formatTimestamp(sb.GetExpiresAt()),
			connected,
		}
	}

	tb := table.New(w, table.Options{Color: useColor})
	tb.Header("NAME", "ID", "STATE", "NETWORK", "IMAGE", "LAUNCHER", "EXPIRES", "CONNECTED")
	for _, r := range rows {
		tb.Row(r...)
	}
	return tb.Render()
}

// ─── rm ──────────────────────────────────────────────────────────────────────

func newSandboxRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name|id>",
		Aliases: []string{"remove"},
		Short:   "Remove a sandbox",
		Long: `Remove a sandbox by name or row id.

Named volumes mounted into the sandbox are never removed; an anonymous volume
(rw with no source) goes with the container.`,
		Args: cobra.ExactArgs(1),
		RunE: runSandboxRemove,
	}
}

func runSandboxRemove(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	if _, err := ep.control().RemoveSandbox(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.RemoveSandboxRequest{Ref: args[0]})); err != nil {
		return connectVerbErr(err, ep.describe)
	}
	fmt.Printf("Sandbox %s removed.\n", args[0])
	return nil
}

// ─── flag parsing ────────────────────────────────────────────────────────────

// parseSandboxMounts parses the repeatable --mount values into wire mounts.
func parseSandboxMounts(values []string) ([]*rafikiv1.SandboxMount, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]*rafikiv1.SandboxMount, len(values))
	for i, v := range values {
		m, err := parseSandboxMount(v)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

// parseSandboxMount parses one --mount value:
//
//	<kind>:<target>[=host:<path>|volume:<name>]
//
// The source, when present, is located by its "=host:" or "=volume:" marker
// rather than by the first "=", so a target or source value that itself
// contains "=" (e.g. a host path "/srv/a=b") is not mistaken for a second
// source. A value carrying BOTH markers, or an unrecognised source shape, is a
// parse error naming the flag value. A missing or unknown kind and a missing
// target are refused too. Deeper validation (absolute clean paths, launcher
// mount roots, ephemeral forbidding a source) is the daemon's, so it stays in
// one place.
func parseSandboxMount(value string) (*rafikiv1.SandboxMount, error) {
	hostIdx := strings.Index(value, "=host:")
	volIdx := strings.Index(value, "=volume:")
	if hostIdx >= 0 && volIdx >= 0 {
		return nil, fmt.Errorf("--mount %q: a mount takes at most one of host:<path> or volume:<name>", value)
	}

	spec := value
	source := ""
	hasSource := false
	switch {
	case hostIdx >= 0:
		spec, source, hasSource = value[:hostIdx], value[hostIdx+1:], true
	case volIdx >= 0:
		spec, source, hasSource = value[:volIdx], value[volIdx+1:], true
	}

	kind, target, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("--mount %q: missing kind (want <kind>:<target>, kind is ro, rw or ephemeral)", value)
	}
	switch kind {
	case string(protocol.MountRO), string(protocol.MountRW), string(protocol.MountEphemeral):
	default:
		return nil, fmt.Errorf("--mount %q: unknown mount kind %q (want ro, rw or ephemeral)", value, kind)
	}
	if target == "" {
		return nil, fmt.Errorf("--mount %q: missing target", value)
	}

	m := &rafikiv1.SandboxMount{Kind: kind, Target: target}
	if !hasSource {
		// A bare "=" with no recognised "=host:"/"=volume:" marker is a
		// malformed source, not a "=" inside a target: name what followed it.
		eq := strings.Index(value, "=")
		if eq < 0 {
			return m, nil
		}
		seg, _, hasColon := strings.Cut(value[eq+1:], ":")
		if hasColon && seg != "host" && seg != "volume" && seg != "" {
			return nil, fmt.Errorf("--mount %q: unknown source %q (want host:<path> or volume:<name>)", value, seg)
		}
		return nil, fmt.Errorf("--mount %q: source must be host:<path> or volume:<name>", value)
	}

	srcKind, srcValue, ok := strings.Cut(source, ":")
	if !ok || srcValue == "" {
		return nil, fmt.Errorf("--mount %q: source must be host:<path> or volume:<name>", value)
	}
	switch srcKind {
	case "host":
		m.HostPath = srcValue
	case "volume":
		m.Volume = srcValue
	default:
		return nil, fmt.Errorf("--mount %q: unknown source %q (want host:<path> or volume:<name>)", value, srcKind)
	}
	return m, nil
}

// parseSandboxMemoryBytes parses --memory: a plain byte count, or a value with
// a binary suffix (case-insensitive) — b, k/kb/kib, m/mb/mib, g/gb/gib,
// t/tb/tib, each a power of 1024. "1g" is 1073741824. A negative, non-finite
// ("inf"/"nan", with or without a suffix), or int64-overflowing value is
// refused, naming the flag.
func parseSandboxMemoryBytes(value string) (int64, error) {
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("--memory %q must not be negative", value)
		}
		return n, nil
	}

	lower := strings.ToLower(strings.TrimSpace(value))
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
		{"b", 1},
	}
	num, mult := "", int64(0)
	for _, s := range suffixes {
		if strings.HasSuffix(lower, s.suffix) {
			num, mult = strings.TrimSuffix(lower, s.suffix), s.mult
			break
		}
	}
	if mult == 0 {
		return 0, fmt.Errorf("--memory %q is not a byte count or a size with a b/k/m/g/t suffix", value)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("--memory %q: %q is not a number", value, num)
	}
	// ParseFloat accepts "inf"/"nan", and int64(f*mult) of those (or of an
	// overflowing product) is implementation-defined — refuse them explicitly
	// rather than hand the daemon a garbage byte count.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("--memory %q is not a finite size", value)
	}
	if f < 0 {
		return 0, fmt.Errorf("--memory %q must not be negative", value)
	}
	product := f * float64(mult)
	// float64(math.MaxInt64) is 2^63; anything at or above it cannot be
	// converted to an int64 byte count.
	if product >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("--memory %q is too large (overflows an int64 byte count)", value)
	}
	return int64(product), nil
}

// parseEnvPairs parses a slice of "K=V" strings into a map. Unlike
// parseLabelPairs it does not impose the label key charset or the rafiki/
// reservation — env keys are the container's, and the daemon applies its own
// RAFIKI_ reservation — but an empty key or a missing "=" is still refused.
func parseEnvPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not in K=V format", pair)
		}
		if k == "" {
			return nil, fmt.Errorf("%q has an empty key", pair)
		}
		out[k] = v
	}
	return out, nil
}
