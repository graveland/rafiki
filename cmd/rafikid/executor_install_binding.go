// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorInstallController is the slice of *Controller the install binding
// drives. *Controller satisfies it directly; the interface exists so both faces
// of this capability — this binding and connectExecutorInstaller — can be driven
// by the SAME object in one test, which is what pins that a caller gets the same
// admission and the same refusal whichever face it arrives on.
type executorInstallController interface {
	InstallExecutor(
		ctx context.Context, caller installCaller, req *rafikiv1.InstallExecutorRequest,
	) (*rafikiv1.ExecutorRow, error)
	UninstallExecutor(ctx context.Context, caller installCaller, ref string) error
}

var _ executorInstallController = (*Controller)(nil)

// executorInstallBinding implements tools.ExecutorInstaller for ONE caller.
//
// It follows the same binding rule as controllerSandboxes and controllerSpawner:
// the caller — a child id and its owner, or just an owner for a human — is
// closed over at construction and appears in no method signature. That is the
// design, not an implementation detail: an identity passed as an argument is a
// tool argument, and a tool argument is produced by an LLM that can be
// prompt-injected into naming somebody else.
//
// The caller is built from the CONNECTION's identity on the MCP face
// (installCallerFromContext, the very function the Connect adapter uses), so the
// two faces cannot disagree about what a credential is.
type executorInstallBinding struct {
	c      executorInstallController
	caller installCaller
}

var _ tools.ExecutorInstaller = (*executorInstallBinding)(nil)

// newExecutorInstallBinding binds one caller to the Controller's install verbs.
func newExecutorInstallBinding(c *Controller, caller installCaller) tools.ExecutorInstaller {
	return &executorInstallBinding{c: c, caller: caller}
}

func (b *executorInstallBinding) Install(
	ctx context.Context, req protocol.ExecutorInstallRequest,
) (protocol.ExecutorInstallResult, error) {
	row, err := b.c.InstallExecutor(ctx, b.caller, &rafikiv1.InstallExecutorRequest{
		Target: req.Target,
		Via:    req.Via,
		Name:   req.Name,
		Root:   req.Root,
		Labels: req.Labels,
		Admits: req.Admits,
		// Mode is the model's transport choice and rides through unchecked: the
		// daemon owns which values are legal and what each means for a target,
		// so a copy of that rule on a second face is a copy that can drift.
		Mode: req.Mode,
	})
	if err != nil {
		return protocol.ExecutorInstallResult{}, err
	}
	if row == nil {
		// An installer that answers without a row is a wiring bug; a tool that
		// reported success would then hand the model an empty id to act on.
		return protocol.ExecutorInstallResult{}, errors.New("the daemon returned no executor row")
	}
	return protocol.ExecutorInstallResult{
		ID:        row.GetId(),
		Name:      row.GetMachine(),
		Via:       row.GetInstalledVia(),
		Target:    row.GetInstalledTarget(),
		Connected: row.GetConnected(),
	}, nil
}

func (b *executorInstallBinding) Uninstall(ctx context.Context, executor string) error {
	return b.c.UninstallExecutor(ctx, b.caller, executor)
}

// installStoreReady reports whether this daemon can install executors at all.
//
// The launch RECIPE table is what makes the two verbs more than an ssh helper
// with no memory: without it an install could neither relaunch a down executor
// nor know what it may uninstall. A daemon that lacks it declines the tools
// (nil-means-decline) rather than advertising verbs that can only answer
// Unavailable — the same rule Sandboxes and Presets follow.
func (c *Controller) installStoreReady() bool {
	return c != nil && c.install != nil && c.install.store != nil
}
