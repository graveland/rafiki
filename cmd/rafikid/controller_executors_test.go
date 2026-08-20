package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// fakeExecStore is an in-memory executors.Store for testing the control-plane
// verbs without a database. It enforces nothing beyond recording calls — the
// real enforcement lives in the postgres store and is covered by its own
// conformance suite.
type fakeExecStore struct {
	minted      []executors.NewToken
	execs       map[string]executors.Executor
	seq         int
	createCalls int
	lastCreate  executors.NewToken
	disabled    []string
}

func newFakeExecStore() *fakeExecStore {
	return &fakeExecStore{execs: map[string]executors.Executor{}}
}

func (f *fakeExecStore) Create(_ context.Context, t executors.NewToken) (executors.Executor, string, error) {
	f.createCalls++
	f.lastCreate = t
	e := executors.Executor{ID: "exec-created", Labels: t.Labels, Enabled: true}
	f.execs[e.ID] = e
	return e, "credential", nil
}

func (f *fakeExecStore) MintToken(_ context.Context, t executors.NewToken) (string, error) {
	f.minted = append(f.minted, t)
	return "tok-1", nil
}
func (f *fakeExecStore) Enroll(_ context.Context, _ string, _ map[string]string) (executors.Executor, string, error) {
	f.seq++
	e := executors.Executor{ID: "exec-" + string(rune('0'+f.seq)), Labels: map[string]string{}}
	f.execs[e.ID] = e
	return e, "cred", nil
}
func (f *fakeExecStore) Authenticate(_ context.Context, _ string) (executors.Executor, error) {
	return executors.Executor{}, nil
}
func (f *fakeExecStore) Get(_ context.Context, id string) (executors.Executor, error) {
	return f.execs[id], nil
}
func (f *fakeExecStore) List(_ context.Context) ([]executors.Executor, error) {
	var out []executors.Executor
	for _, e := range f.execs {
		out = append(out, e)
	}
	return out, nil
}
func (f *fakeExecStore) SetLabels(_ context.Context, id string, set map[string]string, remove []string) (executors.Executor, error) {
	e := f.execs[id]
	if e.Labels == nil {
		e.Labels = map[string]string{}
	}
	for k, v := range set {
		e.Labels[k] = v
	}
	for _, k := range remove {
		delete(e.Labels, k)
	}
	f.execs[id] = e
	return e, nil
}
func (f *fakeExecStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	if !enabled {
		f.disabled = append(f.disabled, id)
	}
	return nil
}
func (f *fakeExecStore) Annotate(_ context.Context, id string, set map[string]string, remove []string) error {
	return nil
}
func (f *fakeExecStore) TouchSeen(_ context.Context, id string) error { return nil }

func TestExecutorEnrollMintsATokenWithTheGivenLabels(t *testing.T) {
	s := newFakeExecStore()
	c := &Controller{execStore: s}

	resp, err := c.ExecutorEnroll(protocol.ExecutorEnrollRequest{
		Labels:     map[string]string{"env": "work"},
		TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("enroll returned an empty token")
	}
	if len(s.minted) != 1 || s.minted[0].Labels["env"] != "work" {
		t.Fatalf("minted token did not carry the labels: %+v", s.minted)
	}
}

func TestExecutorVerbsRefuseWithoutAStore(t *testing.T) {
	c := &Controller{} // execStore nil
	if _, err := c.ExecutorEnroll(protocol.ExecutorEnrollRequest{TTLSeconds: 1}); err == nil {
		t.Fatal("enroll must refuse without a store")
	}
	if _, err := c.ExecutorList(protocol.ExecutorListRequest{}); err == nil {
		t.Fatal("list must refuse without a store")
	}
	if _, err := c.ExecutorLabel(protocol.ExecutorLabelRequest{}); err == nil {
		t.Fatal("label must refuse without a store")
	}
	if err := c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "x"}); err == nil {
		t.Fatal("disable must refuse without a store")
	}
	if err := c.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "x"}); err == nil {
		t.Fatal("enable must refuse without a store")
	}
}

func TestExecutorLabelRoundTrips(t *testing.T) {
	s := newFakeExecStore()
	c := &Controller{execStore: s}
	e0, _, _ := s.Enroll(context.Background(), "tok", nil)
	e, err := c.ExecutorLabel(protocol.ExecutorLabelRequest{
		ExecutorID: e0.ID, Set: map[string]string{"os": "linux"},
	})
	if err != nil {
		t.Fatalf("label: %v", err)
	}
	if e.Labels["os"] != "linux" {
		t.Fatalf("label did not round-trip: %+v", e.Labels)
	}
}

func TestExecutorListFiltersBySelector(t *testing.T) {
	s := newFakeExecStore()
	c := &Controller{execStore: s}
	s.execs["exec-work"] = executors.Executor{ID: "exec-work", Labels: map[string]string{"env": "work"}}
	s.execs["exec-home"] = executors.Executor{ID: "exec-home", Labels: map[string]string{"env": "home"}}

	execs, err := c.ExecutorList(protocol.ExecutorListRequest{Selector: "env=work"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(execs) != 1 || execs[0].ID != "exec-work" {
		t.Fatalf("selector filtered wrong: %+v", execs)
	}
}

// Connected is a view over the live pool, not the store. A client waiting for
// its own session executor to connect has no other signal; the row alone cannot
// say whether it is up.
func TestExecutorListMarksConnectedFromTheLivePool(t *testing.T) {
	s := newFakeExecStore()
	s.execs["exec-live"] = executors.Executor{ID: "exec-live", Enabled: true}
	s.execs["exec-off"] = executors.Executor{ID: "exec-off", Enabled: true}
	c := &Controller{execStore: s, execPool: &fakePool{live: []execpool.LiveExecutor{ex("exec-live", nil, "")}}}

	execs, err := c.ExecutorList(protocol.ExecutorListRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	connected := map[string]bool{}
	for _, e := range execs {
		connected[e.ID] = e.Connected
	}
	if !connected["exec-live"] {
		t.Error("exec-live not marked connected though the pool has it live")
	}
	if connected["exec-off"] {
		t.Error("exec-off marked connected though the pool does not have it")
	}
}
