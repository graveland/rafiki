package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// userSpawner implements tools.AgentSpawner for one authenticated USER.
//
// owner is closed over at construction and never arrives as an argument, for
// the same reason controllerSpawner's selfID is: an id in a method signature
// is one refactor away from being a tool argument, and a tool argument is
// produced by an LLM that can be prompt-injected.
//
// Unlike controllerSpawner there is no lineage to read, so the verbs that
// consult IsDescendant here consult nothing: rafiki has no per-user ownership
// filter to reuse, and this surface has the daemon's existing single-operator
// posture. When ownership filtering arrives it lands in THIS type as a
// predicate over childstore.Snapshot — owner_user_id is already a column on
// conversations.child.
type userSpawner struct {
	c     *Controller
	owner users.Identity
}

var _ tools.AgentSpawner = (*userSpawner)(nil)

func newUserSpawner(c *Controller, owner users.Identity) *userSpawner {
	return &userSpawner{c: c, owner: owner}
}

// List reports every child the daemon knows, at absolute depth rather than
// hops from a binding: an MCP caller is not a node in the tree, so "hops from
// me" is undefined, while "depth in the daemon's forest" renders the same
// nesting `rafiki list` shows.
func (s *userSpawner) List(context.Context) ([]tools.AgentInfo, error) {
	snaps := s.c.st.List()
	out := make([]tools.AgentInfo, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, snapshotInfo(snap, s.c.st.AbsoluteDepth(snap.ChildID)))
	}
	sortAgents(out)
	return out, nil
}

func (s *userSpawner) Models(ctx context.Context, q tools.ModelQuery) ([]tools.ModelInfo, error) {
	return spawnerModels(ctx, s.c, q)
}

// Spawn starts a TOP-LEVEL child owned by the bound user. ParentChildID,
// Task and SpawnerConversationID are deliberately unset: there is no parent,
// and SpawnerConversationID exists only to resolve a task handle against the
// caller's own ledger — an MCP caller's ledger is resolved per request by the
// bridge, not by the controller.
func (s *userSpawner) Spawn(ctx context.Context, spec tools.SpawnSpec) (tools.AgentInfo, error) {
	kind := spec.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	if spec.Cwd == "" || !filepath.IsAbs(spec.Cwd) {
		return tools.AgentInfo{}, errors.New("cwd is required and must be an absolute path")
	}
	// Refused before anything is built or admitted, so a refusal starts no
	// process.
	if spec.Task != "" {
		return tools.AgentInfo{}, errors.New("task assignment at spawn is not available on this surface; spawn the agent, then assign the task with task_update")
	}
	// pkg/control's dispatch validates cwd absoluteness for the framed path
	// and MCP does not go through it, while Controller.Spawn only stats cwd
	// for kinds it forks locally — so this surface carries the check itself.
	req := protocol.SpawnRequest{
		Type:             protocol.TypeCtrlSpawn,
		Kind:             kind,
		Name:             spec.Name,
		Model:            spec.Model,
		Cwd:              spec.Cwd,
		MaxDepth:         spec.MaxDepth,
		MaxCost:          spec.MaxCost,
		MaxChildren:      spec.MaxChildren,
		ExecutorSelector: spec.ExecutorSelector,
		WorkspaceMode:    spec.WorkspaceMode,
	}
	res, err := s.c.Spawn(ctx, req, s.owner)
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
	return snapshotInfo(snap, s.c.st.AbsoluteDepth(res.ChildID)), nil
}

func (s *userSpawner) View(_ context.Context, childID string, limit int) (string, error) {
	if childID == "" {
		return "", errors.New("agent id is required")
	}
	if _, ok := s.c.st.Get(childID); !ok {
		return "", fmt.Errorf("agent %s is not registered", childID)
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

func (s *userSpawner) Send(_ context.Context, childID, message string) error {
	if childID == "" {
		return errors.New("agent id is required")
	}
	if message == "" {
		return errors.New("message is required")
	}
	if _, ok := s.c.st.Get(childID); !ok {
		return fmt.Errorf("agent %s is not registered", childID)
	}
	frame, err := json.Marshal(map[string]string{"type": "prompt", "message": message})
	if err != nil {
		return err
	}
	return s.c.Send(childID, frame)
}

// Kill shuts a child down and waits for the exit to be recorded. It
// deliberately does NOT mark selfKilled: that marker suppresses the exit
// notice to the child's own parent, which is right when a coordinator kills
// its own worker and wrong here — an operator killing somebody's worker is
// exactly the case where that worker's parent should be told.
func (s *userSpawner) Kill(ctx context.Context, childID string) error {
	if childID == "" {
		return errors.New("agent id is required")
	}
	if _, ok := s.c.st.Get(childID); !ok {
		return fmt.Errorf("agent %s is not registered", childID)
	}
	if _, err := s.c.Kill(ctx, childID, 0, 0); err != nil {
		return err
	}
	if !waitForChildRemoval(s.c.cm, childID, killWaitTimeout) {
		return fmt.Errorf("agent %s did not finish shutting down within %s", childID, killWaitTimeout)
	}
	return nil
}

// SetBudget changes a TOP-LEVEL child's budget. Unlike controllerSpawner's
// direct-child rule, the refusal here is any parented child at all — a
// parented child's budget belongs to the agent that spawned it, and the
// caller is told to go through that agent (or the top-level root of the
// subtree) instead.
func (s *userSpawner) SetBudget(ctx context.Context, childID string, maxCost float64) error {
	if childID == "" {
		return errors.New("agent id is required")
	}
	if parent, ok := s.c.st.ParentOf(childID); ok && parent != "" {
		return fmt.Errorf(
			"agent %s was spawned by agent %s; change its budget through that agent, or set the budget of the top-level agent that owns the subtree",
			childID, parent)
	}
	return s.c.SetChildBudgetAsOperator(ctx, childID, maxCost)
}
