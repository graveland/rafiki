package main

import (
	"context"
	"fmt"
	"log/slog"
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
		return c.resolvePoolessExecutorSet(req)
	}

	// Admits is read against the CHILD's labels, the same ones Reachable and
	// ChooseFor read, so the set stored here is one reach will not then refuse.
	childLabels := c.admissionLabels(req, owner)
	rows := c.spawnerCandidateRows(owner)
	parentRows, err := c.effectiveExecutorSetOver(rows, req.ParentChildID, childLabels, owner.UserID)
	if err != nil {
		return protocol.SpawnRequest{}, err
	}
	// Only what the spawner's reach covers can be handed on, and its reach never
	// includes another child's sandbox. An operator's own sandbox stays
	// nameable: a spawner may name it explicitly, but a selector or a default
	// never lands a child on one when the spawner holds "all".
	parentRows = dropChildOwnedSandboxes(parentRows)
	parentAll, _, _ := c.executorSetSpec(req.ParentChildID)
	yours := executorDisplayNames(parentRows)
	if parentAll {
		yours = childstore.ExecutorSetAll
	}

	// A selector or an ephemeral spawn PLACES the child on whatever can host it, so
	// it freezes only executors that are connected or installed over ssh (the
	// daemon can bring those back). An enrolled executor that is merely offline
	// is a valid name and a valid default member, but a selector must not freeze
	// every stale row of the owner that happens to match it.
	placementRows := c.placementCandidates(parentRows)

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
		// Every spawner-set member that matches, offline installed ones included,
		// is frozen into the child's set: an offline member is still a name the
		// child can call, so it must not be dropped or turn the spawn into a
		// refusal. Only a selector that matches nothing anywhere refuses.
		sel, perr := executors.ParseSelector(req.ExecutorSelector)
		if perr != nil {
			return protocol.SpawnRequest{}, setRefusal("invalid executor selector %q: %s", req.ExecutorSelector, perr)
		}
		matched := narrowByWorkspaceMode(executors.Narrow(placementRows, sel), req.WorkspaceMode)
		if parentAll {
			matched = dropSandboxCandidates(matched)
		}
		if len(matched) == 0 {
			return protocol.SpawnRequest{}, setRefusal("%s",
				c.explainNoMatch(req, sel, parentRows, childLabels, owner.UserID).Error())
		}
		sortCandidates(matched)
		result = idsOf(matched)
		// A session executor (the transient one `rafiki create` stands up for the
		// operator's terminal) is placement, never membership: it is evicted when
		// the terminal closes, so freezing its id would leave the child and its
		// workers addressing nothing. The child instead keeps the spawner's whole
		// reach and starts out working on the session executor.
		if transient := transientRows(matched); len(transient) > 0 {
			stable := idsOf(dropTransientRows(matched))
			switch {
			case parentAll:
				result = []string{childstore.ExecutorSetAll}
			case len(stable) > 0:
				result = stable
			default:
				return protocol.SpawnRequest{}, setRefusal("executor selector %q matches only a session executor, which ends with its terminal and cannot be the whole of a set; name a durable executor", req.ExecutorSelector)
			}
			if req.HomeExecutor == "" {
				req.HomeExecutor = transient[0].ID
			}
		}

	case req.WorkspaceMode == "ephemeral":
		// An ephemeral child must keep somewhere to be re-placed when its executor
		// is lost, so it never gets a single home.
		eligible := dropSandboxCandidates(narrowByWorkspaceMode(placementRows, "ephemeral"))
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

// placementCandidates keeps the rows a selector or an ephemeral spawn may
// freeze: connected ones and ones installed over ssh.
func (c *Controller) placementCandidates(rows []executors.Executor) []executors.Executor {
	live := make(map[string]bool)
	for _, le := range c.execPool.Live() {
		live[le.Executor.ID] = true
	}
	var out []executors.Executor
	for _, e := range rows {
		if live[e.ID] || c.isInstalledExecutor(e.ID) {
			out = append(out, e)
		}
	}
	return out
}

// transientRows are the session executors in rows: the ones that end with the
// terminal that started them.
func transientRows(rows []executors.Executor) []executors.Executor {
	var out []executors.Executor
	for _, e := range rows {
		if e.Labels["kind"] == "session" {
			out = append(out, e)
		}
	}
	return out
}

// dropTransientRows is rows without the session executors.
func dropTransientRows(rows []executors.Executor) []executors.Executor {
	var out []executors.Executor
	for _, e := range rows {
		if e.Labels["kind"] != "session" {
			out = append(out, e)
		}
	}
	return out
}

// resolvePoolessExecutorSet fixes the set of a spawn on a daemon with no
// executor pool. Nothing can be verified against a live or installed executor
// there, so the child inherits its spawner's STORED set (every set is
// ["all"] at the top level) and any request to name one is refused. A fresh
// spawn never stores nil: a nil set reads as a legacy row that addresses
// every executor its owner has, which a pool enabled later would honour.
func (c *Controller) resolvePoolessExecutorSet(req protocol.SpawnRequest) (protocol.SpawnRequest, error) {
	all, ids, selector := c.executorSetSpec(req.ParentChildID)
	if req.ParentChildID != "" {
		if _, ok := c.st.Get(req.ParentChildID); !ok {
			return protocol.SpawnRequest{}, setRefusal("cannot read the executor set of parent %s: its row is not in the store", req.ParentChildID)
		}
	}

	var named []string
	for _, entry := range req.Executors {
		if entry = strings.TrimSpace(entry); entry != "" {
			named = append(named, entry)
		}
	}
	switch {
	case len(named) == 1 && named[0] == childstore.ExecutorSetAll:
		if !all {
			return protocol.SpawnRequest{}, setRefusal(`"all" is available only to an agent whose own executor set is all`)
		}
	case len(named) > 0 || req.ExecutorRef != "" || strings.TrimSpace(req.ExecutorSelector) != "":
		return protocol.SpawnRequest{}, setRefusal("this daemon has no executor pool, so a named executor cannot be verified; omit the executor to inherit the spawner's set")
	}

	switch {
	case all:
		req.Executors = []string{childstore.ExecutorSetAll}
	case strings.TrimSpace(selector) != "":
		// A legacy selector cannot be written as ids without a pool to resolve it.
		req.Executors = []string{}
	default:
		req.Executors = slices.Clone(ids)
		if req.Executors == nil {
			req.Executors = []string{}
		}
	}
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
// set narrows them: the live pool plus every other enrolled executor of the
// owner. A set member that is offline is still a valid name and must survive
// into a child's set, so reading only live rows would store [] for a spawner
// whose members are all down.
func (c *Controller) spawnerCandidateRows(owner executorOwner) []executors.Executor {
	live := c.execPool.Live()
	rows := make([]executors.Executor, 0, len(live))
	have := make(map[string]bool, len(live))
	for _, le := range live {
		rows = append(rows, le.Executor)
		have[le.Executor.ID] = true
	}
	for _, e := range c.offlineEnrolledRows(owner) {
		if !have[e.ID] {
			rows = append(rows, e)
		}
	}
	return rows
}

// offlineEnrolledRows is the owner's enrolled executors, read from their
// durable rows. A read failure adds nothing and is logged: a member that cannot
// be read is simply not offered, and the spawn then refuses it by name.
func (c *Controller) offlineEnrolledRows(owner executorOwner) []executors.Executor {
	if c.execStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), installLookupTimeout)
	defer cancel()
	rows, err := c.execStore.List(ctx)
	if err != nil {
		slog.Warn("could not list enrolled executors; only live ones are offered to a new child",
			"owner", owner.Name, "error", err)
		return nil
	}
	var mine []executors.Executor
	for _, e := range rows {
		if e.OwnerUserID == owner.UserID {
			mine = append(mine, e)
		}
	}
	return mine
}
