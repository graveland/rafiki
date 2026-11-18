package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorSetSpec is childID's stored executor set, interpreted. "" is a
// top-level caller, which may address every executor of its owner. An
// unknown child addresses nothing: every caller asks a narrowing question,
// so failing to find the row must withhold reach, never grant it.
func (c *Controller) executorSetSpec(childID string) (all bool, ids []string, selector string) {
	if childID == "" {
		return true, nil, ""
	}
	if c.st == nil {
		return false, []string{}, ""
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return false, []string{}, ""
	}
	return childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector)
}

// boundSetSpec is the set spec for a binder's child. The stored row is the
// authority once it exists. A fresh spawn's eager bind runs before its row is
// stored, so there the request carries the set resolveSpawnExecutorSet fixed; a
// request that carries none (nil) addresses nothing, never everything.
func (c *Controller) boundSetSpec(req protocol.SpawnRequest, childID string) (all bool, ids []string, selector string) {
	if c.st != nil {
		if _, ok := c.st.Get(childID); ok {
			return c.executorSetSpec(childID)
		}
	}
	if req.Executors == nil {
		return false, []string{}, ""
	}
	return childstore.ResolveSetSpec(req.Executors, "")
}

// filterBySet keeps the rows a set spec admits. A legacy selector that does
// not parse admits nothing, and so does an id list that is empty or absent.
func filterBySet(rows []executors.Executor, all bool, ids []string, selector string) []executors.Executor {
	if all {
		return rows
	}
	if strings.TrimSpace(selector) != "" {
		sel, err := executors.ParseSelector(selector)
		if err != nil {
			return nil
		}
		return executors.Narrow(rows, sel)
	}
	var kept []executors.Executor
	for _, e := range rows {
		if slices.Contains(ids, e.ID) {
			kept = append(kept, e)
		}
	}
	return kept
}

// idsOf returns the ids of rows in input order. The result is never nil: a
// stored nil set is a legacy row that addresses everything, whereas an empty
// one addresses nothing.
func idsOf(rows []executors.Executor) []string {
	out := make([]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.ID)
	}
	return out
}

// setRefusal is the refusal every resolveSpawnExecutorSet failure returns.
func setRefusal(format string, args ...any) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: "executor set: " + fmt.Sprintf(format, args...),
	}
}

// executorDisplayNames is the sorted, comma-joined machine labels (else short
// ids) of rows, for a refusal that names what the caller may use.
func executorDisplayNames(rows []executors.Executor) string {
	if len(rows) == 0 {
		return "none"
	}
	names := make([]string, 0, len(rows))
	for _, e := range rows {
		if m := e.Labels["machine"]; m != "" {
			names = append(names, m)
			continue
		}
		names = append(names, shortID(e.ID))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// resolveSpawnExecutorSet fixes the new child's executor set, always a
// subset of the spawner's, and writes it to req.Executors as ids or
// [childstore.ExecutorSetAll]. It runs while nothing is minted, so a
// refusal starts no process.
//
// It runs on FRESH spawns only. A resumed or respawned child's set is the one
// stored on its row: req.Executors is a repeated field where empty means "the
// daemon chooses", so re-resolving it would widen a stored empty or legacy set.
func (c *Controller) resolveSpawnExecutorSet(req protocol.SpawnRequest, owner executorOwner) (protocol.SpawnRequest, error) {
	if c.execPool == nil {
		return req, nil
	}

	childLabels := c.admissionLabels(req, owner)
	rows := c.spawnerCandidateRows(owner)
	parentRows, err := c.effectiveExecutorSetOver(rows, req.ParentChildID, childLabels, owner.UserID)
	if err != nil {
		return protocol.SpawnRequest{}, err
	}
	parentAll, _, _ := c.executorSetSpec(req.ParentChildID)
	yours := executorDisplayNames(parentRows)
	if parentAll {
		yours = childstore.ExecutorSetAll
	}

	var named []string
	for _, entry := range req.Executors {
		if entry = strings.TrimSpace(entry); entry != "" {
			named = append(named, entry)
		}
	}

	// resolveRefs maps typed names onto the spawner's own rows. A name outside
	// them is refused, so a stored set can never exceed the spawner's.
	resolveRefs := func(refs []string) ([]string, error) {
		ids := make([]string, 0, len(refs))
		for _, ref := range refs {
			hit, ok := matchExecutorRef(ref, parentRows)
			if !ok {
				return nil, setRefusal("executor %q is not in your executor set; yours: %s", ref, yours)
			}
			if !slices.Contains(ids, hit.ID) {
				ids = append(ids, hit.ID)
			}
		}
		return ids, nil
	}

	var result []string
	switch {
	case len(named) > 0:
		if slices.Contains(named, childstore.ExecutorSetAll) {
			if len(named) != 1 {
				return protocol.SpawnRequest{}, setRefusal(`"all" cannot be combined with other names`)
			}
			if !parentAll {
				return protocol.SpawnRequest{}, setRefusal(`"all" is available only to an agent whose own executor set is all`)
			}
			result = []string{childstore.ExecutorSetAll}
			break
		}
		if result, err = resolveRefs(named); err != nil {
			return protocol.SpawnRequest{}, err
		}

	case req.ExecutorRef != "":
		if result, err = resolveRefs([]string{req.ExecutorRef}); err != nil {
			return protocol.SpawnRequest{}, err
		}

	case strings.TrimSpace(req.ExecutorSelector) != "":
		// req.Executors is not resolved yet, so this narrows the spawner's set by
		// the selector alone; every match is frozen into the child's set.
		probe := req
		probe.Executors = nil
		candidates, parentSet, labels, sel, nerr := c.narrowedExecutorCandidates(probe, owner)
		if nerr != nil {
			return protocol.SpawnRequest{}, setRefusal("%s", nerr.Error())
		}
		if len(candidates) == 0 {
			return protocol.SpawnRequest{}, setRefusal("%s", c.explainNoMatch(probe, sel, parentSet, labels, owner.UserID).Error())
		}
		result = idsOf(candidates)

	case req.WorkspaceMode == "ephemeral":
		// An ephemeral child must keep somewhere to be re-placed when its executor
		// is lost, so it never gets a single home.
		eligible := dropSandboxCandidates(narrowByWorkspaceMode(parentRows, "ephemeral"))
		if len(eligible) == 0 {
			return protocol.SpawnRequest{}, setRefusal("%s",
				c.explainNoMatch(req, executors.Selector{}, parentRows, childLabels, owner.UserID).Error())
		}
		result = idsOf(eligible)

	case req.ParentChildID == "":
		result = []string{childstore.ExecutorSetAll}

	default:
		result = c.defaultChildSet(req, parentRows, parentAll)
	}

	req.Executors = result
	return req, nil
}

// defaultChildSet is the set a child gets when it names nothing: the spawner's
// home when it can serve the child's kind, else a copy of the spawner's set.
func (c *Controller) defaultChildSet(req protocol.SpawnRequest, parentRows []executors.Executor, parentAll bool) []string {
	if home := c.persistedHome(req.ParentChildID); home != "" {
		inSet := false
		for _, e := range parentRows {
			if e.ID == home {
				inSet = true
				break
			}
		}
		launches := req.Kind != protocol.KindClaude && req.Kind != protocol.KindScript
		if !launches {
			launches = launchKindSet(c.execPool.Live(), string(req.Kind))[home]
		}
		if inSet && launches {
			return []string{home}
		}
	}
	if parentAll {
		return []string{childstore.ExecutorSetAll}
	}
	return idsOf(parentRows)
}

// spawnerCandidateRows is every row the spawner could address before its own
// set narrows them: the live pool plus the owner's offline installed
// executors.
func (c *Controller) spawnerCandidateRows(owner executorOwner) []executors.Executor {
	live := c.execPool.Live()
	rows := make([]executors.Executor, 0, len(live))
	have := make(map[string]bool, len(live))
	for _, le := range live {
		rows = append(rows, le.Executor)
		have[le.Executor.ID] = true
	}
	for _, e := range c.offlineInstalledRows(owner) {
		if !have[e.ID] {
			rows = append(rows, e)
		}
	}
	return rows
}
