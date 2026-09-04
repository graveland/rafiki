package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelquery"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"
)

// controllerSpawner implements tools.AgentSpawner for one child.
//
// selfID is closed over at construction and never arrives as an argument.
// That is the whole design: §1.2's rule is not "the check runs in the daemon"
// — fundi children run in-process, so tool and daemon share an address space —
// it is "the check reads stored state, never a tool argument". A self id in a
// method signature is a tool argument one refactor away from being one.
type controllerSpawner struct {
	c      *Controller
	selfID string
}

func newControllerSpawner(c *Controller, selfID string) *controllerSpawner {
	return &controllerSpawner{c: c, selfID: selfID}
}

// killWaitTimeout bounds the wait for cm.Remove after a Kill. A kill is not
// complete when Shutdown returns — the status only reads "exited" once
// handleChildExit has run on monitorChild's goroutine, and cm.Remove is its
// last step. Reporting a kill as done before that is what made the lifecycle
// integration tests look like flakes for months.
const killWaitTimeout = 10 * time.Second

// errNotDescendant is returned by every verb that names another child.
func (s *controllerSpawner) errNotDescendant(target string) error {
	return fmt.Errorf(
		"agent %s is not a descendant of yours; you may only view, steer or kill agents you spawned (or that they spawned). Use agent_list to see your subtree",
		target)
}

// authorize is the single gate. It reads the stored parent chain via
// childstore.IsDescendant, which walks real labels rather than trusting the
// rafiki/root cache, and refuses the caller's own id (a child is not a
// descendant of itself) and any unknown id.
func (s *controllerSpawner) authorize(target string) error {
	if target == "" {
		return errors.New("agent id is required")
	}
	if !s.c.st.IsDescendant(s.selfID, target) {
		return s.errNotDescendant(target)
	}
	return nil
}

func (s *controllerSpawner) List(context.Context) ([]tools.AgentInfo, error) {
	snaps := s.c.st.Descendants(s.selfID)
	out := make([]tools.AgentInfo, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, s.infoFor(snap))
	}
	// Nearest first, then newest first within a depth, so a coordinator's own
	// workers head the list and their reviewers follow.
	sortAgents(out)
	return out, nil
}

func (s *controllerSpawner) infoFor(snap childstore.Snapshot) tools.AgentInfo {
	return tools.AgentInfo{
		ChildID: snap.ChildID,
		Name:    snap.Name,
		Model:   joinModel(snap.Provider, snap.Model),
		Status:  string(snap.Status),
		Cwd:     snap.Cwd,
		Depth:   s.hopsTo(snap.ChildID),
		Task:    snap.Labels[labelTaskHandle],
	}
}

// hopsTo counts parent links from childID up to selfID. Returns 0 when the
// chain does not reach selfID, which authorize has already excluded on every
// caller path.
func (s *controllerSpawner) hopsTo(childID string) int {
	cur := childID
	for hops := 1; hops <= 64; hops++ {
		parent, ok := s.c.st.ParentOf(cur)
		if !ok {
			return 0
		}
		if parent == s.selfID {
			return hops
		}
		cur = parent
	}
	return 0
}

func sortAgents(a []tools.AgentInfo) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Depth != a[j].Depth {
			return a[i].Depth < a[j].Depth
		}
		return a[i].ChildID > a[j].ChildID // ULID prefix: newest first
	})
}

// Models answers agent_models.
//
// It reads ListModelRows, not ListModels: the row path is served from the
// daemon's already-warm routing catalog, carries price/context/score, and
// applies filterRowsForKind -- so a claude child is never offered an
// OpenRouter id it cannot resolve. ListModels would instead run a SECOND
// OpenRouter fetch with its own cache and TTL in the same process, and hand
// back bare ids.
//
// Filtering and ordering go through pkg/modelquery, shared with the cockpit's
// picker, so both surfaces agree on the three absence rules: a bound admits a
// row the catalog cannot answer for, an absent value sorts last in BOTH
// directions, and unknown capability is kept rather than read as "no".
func (s *controllerSpawner) Models(ctx context.Context, q tools.ModelQuery) ([]tools.ModelInfo, error) {
	rows, err := s.c.ListModelRows(ctx, "", q.Kind)
	if err != nil {
		return nil, err
	}

	pb := make([]*rafikiv1.ModelRow, 0, len(rows))
	for _, r := range rows {
		pb = append(pb, connectapi.ToProtoModel(r))
	}

	bounds := modelBounds(q)
	keep := pb[:0]
	for _, r := range pb {
		if !modelquery.AdmitsAll(bounds, r) {
			continue
		}
		if !admitsNeeds(r, q.Needs) {
			continue
		}
		keep = append(keep, r)
	}
	pb = keep

	if q.Sort != "" {
		f, ok := modelquery.ParseField(q.Sort)
		if !ok {
			return nil, fmt.Errorf("unknown sort %q", q.Sort)
		}
		modelquery.Sort(pb, []modelquery.SortKey{{Field: f, Desc: modelquery.BiggerIsBetter(f)}})
	}

	out := make([]tools.ModelInfo, 0, len(pb))
	for _, r := range pb {
		out = append(out, toolModelInfo(r))
	}
	return out, nil
}

// modelBounds turns the tool's plain numeric limits into shared constraints.
// Prices arrive per MILLION tokens, which is the unit modelquery works in.
func modelBounds(q tools.ModelQuery) map[modelquery.Field]modelquery.Bound {
	bounds := map[modelquery.Field]modelquery.Bound{}
	if q.MaxInUSD != nil {
		v := *q.MaxInUSD
		bounds[modelquery.FieldPromptUSD] = modelquery.Bound{Max: &v}
	}
	if q.MaxOutUSD != nil {
		v := *q.MaxOutUSD
		bounds[modelquery.FieldCompletionUSD] = modelquery.Bound{Max: &v}
	}
	if q.MinContext != nil {
		v := float64(*q.MinContext)
		bounds[modelquery.FieldContext] = modelquery.Bound{Min: &v}
	}
	return bounds
}

// admitsNeeds applies the capability requirements. A model the catalog cannot
// answer for is KEPT: unknown is not "no", and treating it as no hides every
// locally-served model, none of which has a catalog entry.
func admitsNeeds(r *rafikiv1.ModelRow, needs []string) bool {
	for _, n := range needs {
		var got modelquery.Support
		switch strings.ToLower(n) {
		case "tools":
			got = modelquery.Tools(r)
		case "vision":
			got = modelquery.Vision(r)
		case "reasoning":
			got = modelquery.Reasoning(r)
		default:
			return false // validated in the tool; refuse rather than ignore
		}
		if got == modelquery.SupportNo {
			return false
		}
	}
	return true
}

// toolModelInfo copies a row onto the tool-facing type. Pointers are copied
// through as-is: presence is decided by the catalog, never re-derived, and a
// > 0 guard here would turn a reported zero into absent.
func toolModelInfo(r *rafikiv1.ModelRow) tools.ModelInfo {
	m := tools.ModelInfo{
		ID:       r.GetId(),
		Provider: r.GetProvider(),
		Name:     r.GetName(),
		Tools:    modelquery.Tools(r).String(),
		Vision:   modelquery.Vision(r).String(),
	}
	if r.ContextWindow != nil {
		v := int(*r.ContextWindow)
		m.ContextWindow = &v
	}
	if r.MaxCompletionTokens != nil {
		v := int(*r.MaxCompletionTokens)
		m.MaxCompletionTokens = &v
	}
	m.PromptUSD = r.PromptUsd
	m.CompletionUSD = r.CompletionUsd
	m.CacheReadUSD = r.CacheReadUsd
	m.IntelligenceIndex = r.IntelligenceIndex
	m.CodingIndex = r.CodingIndex
	m.AgenticIndex = r.AgenticIndex
	return m
}

func (s *controllerSpawner) View(ctx context.Context, childID string, limit int) (string, error) {
	if err := s.authorize(childID); err != nil {
		return "", err
	}
	if limit <= 0 || limit > viewMaxEntries {
		limit = viewDefaultEntries
	}
	res, err := s.c.GetRecent(childID, control.RecentQuery{Limit: limit, Rendered: false})
	if err != nil {
		return "", err
	}
	return renderTranscript(res.Events, viewMaxBytes), nil
}

func (s *controllerSpawner) Send(ctx context.Context, childID, message string) error {
	if err := s.authorize(childID); err != nil {
		return err
	}
	if message == "" {
		return errors.New("message is required")
	}
	frame, err := json.Marshal(map[string]string{"type": "prompt", "message": message})
	if err != nil {
		return err
	}
	return s.c.Send(childID, frame)
}

func (s *controllerSpawner) Kill(ctx context.Context, childID string) error {
	if err := s.authorize(childID); err != nil {
		return err
	}
	if _, err := s.c.Kill(ctx, childID, 0, 0); err != nil {
		return err
	}
	// Kill already waits for cm.Remove internally, but a caller that reads
	// on-disk or store state immediately afterwards needs the guarantee
	// restated here rather than inferred.
	if !waitForChildRemoval(s.c.cm, childID, killWaitTimeout) {
		return fmt.Errorf("agent %s did not finish shutting down within %s", childID, killWaitTimeout)
	}
	return nil
}

// Spawn creates a descendant. ParentChildID is the caller's own id, taken
// from the binding — never from spec — so an agent cannot spawn into another
// subtree.
//
// The task assignment rides along INSIDE SpawnRequest rather than being a
// follow-up Assign call. Two reasons: a separate call leaves a window where a
// row is assigned to a child that has not started, and a spawn refused by a
// limit (phase 05) would need a compensating write to undo it. Controller.Spawn
// assigns only after the child is registered, so a refusal writes nothing.
func (s *controllerSpawner) Spawn(ctx context.Context, spec tools.SpawnSpec) (tools.AgentInfo, error) {
	kind := spec.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	self, ok := s.c.st.Get(s.selfID)
	if !ok {
		return tools.AgentInfo{}, fmt.Errorf("spawning agent %s is not registered", s.selfID)
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd = self.Cwd
	}

	req := protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Kind:          kind,
		Name:          spec.Name,
		Model:         spec.Model,
		Cwd:           cwd,
		ParentChildID: s.selfID,
		Task:          spec.Task,
		// SpawnerConversationID lets Controller.Spawn resolve the handle
		// against the CALLER's ledger rather than the new child's, which
		// does not have one yet.
		SpawnerConversationID: self.SessionID,
		MaxDepth:              spec.MaxDepth,
		MaxCost:               spec.MaxCost,
		MaxChildren:           spec.MaxChildren,
		ExecutorSelector:      spec.ExecutorSelector,
		WorkspaceMode:         spec.WorkspaceMode,
	}
	res, err := s.c.Spawn(ctx, req, users.Identity{})
	if err != nil {
		return tools.AgentInfo{}, err
	}
	if spec.Prompt != "" {
		frame, mErr := json.Marshal(map[string]string{"type": "prompt", "message": spec.Prompt})
		if mErr != nil {
			return tools.AgentInfo{}, mErr
		}
		if sErr := s.c.Send(res.ChildID, frame); sErr != nil {
			return tools.AgentInfo{}, fmt.Errorf("agent %s started but its prompt could not be delivered: %w", res.ChildID, sErr)
		}
	}
	snap, _ := s.c.st.Get(res.ChildID)
	return s.infoFor(snap), nil
}

// labelTaskHandle records the ledger handle a child was spawned to work on.
// Daemon-stamped at spawn under the reserved rafiki/ prefix, so a child cannot
// rewrite its own assignment.
const labelTaskHandle = "rafiki/task"

// unused import guard for the tasks package until Task 3 lands.
var _ tasks.Status

// --- transcript rendering ---

const (
	viewDefaultEntries = 40
	viewMaxEntries     = 200
	// viewMaxBytes caps the rendered transcript. A tool result is inlined
	// into the caller's next request, so an unbounded view would let one
	// agent_view call blow the coordinator's context window — a limit the
	// frame cap does not cover, because this never crosses a frame.
	viewMaxBytes = 32 * 1024
)

// renderTranscript flattens pi-vocabulary event frames into plain text a model
// can read. It deliberately renders only what a reader needs to judge what an
// agent did — prompts, assistant text, tool calls and truncated tool results —
// and drops thinking blocks, which are long and rarely load-bearing to an
// observer.
//
// Newest content is what matters, so the byte budget is applied from the END:
// an over-budget transcript loses its oldest entries, not its most recent.
func renderTranscript(events []json.RawMessage, maxBytes int) string {
	var lines []string
	for _, raw := range events {
		var env struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			ToolName string `json:"toolName"`
			Result   struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Type {
		case "message_end":
			if line := renderMessage(env.Message.Role, env.Message.Content); line != "" {
				lines = append(lines, line)
			}
		case "tool_execution_end":
			text := ""
			if len(env.Result.Content) > 0 {
				text = env.Result.Content[0].Text
			}
			status := "ok"
			if env.IsError {
				status = "ERROR"
			}
			lines = append(lines, fmt.Sprintf("  → %s [%s] %s",
				env.ToolName, status, truncateLine(text, 400)))
		}
	}

	// Apply the budget from the end.
	total := 0
	cut := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if total+len(lines[i])+1 > maxBytes {
			break
		}
		total += len(lines[i]) + 1
		cut = i
	}
	var sb strings.Builder
	if cut > 0 {
		fmt.Fprintf(&sb, "[%d earlier entries omitted]\n", cut)
	}
	for _, l := range lines[cut:] {
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "(no transcript yet)\n"
	}
	return sb.String()
}

func renderMessage(role string, content json.RawMessage) string {
	// A user message's content is a bare string (child.PiUserMessage.Content
	// is `any` and carries a string); an assistant message's is a block array.
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return "user: " + truncateLine(text, 2000)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, truncateLine(b.Text, 2000))
			}
		case "toolCall":
			parts = append(parts, "<calls "+b.Name+">")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return role + ": " + strings.Join(parts, " ")
}

func truncateLine(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}
