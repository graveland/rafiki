// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// The Wave-2 seams (ChildOps, ExecutorAdmin, UserAdmin, RawChildIO,
// ExecutorSessions): Set*(nil) must be REFUSED, not stored — the same rule
// SetSkillManager pins. A stored pointer to a nil interface would make
// Load() return a non-nil pointer to a nil manager, defeating the handler's
// Unavailable path and nil-panicking the first handler call.
//
// Unlike TestSetSkillManagerNilIsRefused these cannot go through a handler
// yet: the Wave-1 stubs are unconditional CodeUnimplemented and read no seam.
// The pin is therefore direct — after Set*(nil), the stored pointer must
// still be nil. When Wave 2 fills the handlers these should grow the
// handler-shaped check the skills test uses.
func TestSetChildOpsNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetChildOps(nil)
	assert.NewAborting(t).Nil(s.childOps.Load(), "SetChildOps(nil) stored a pointer")
}

func TestSetExecutorAdminNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetExecutorAdmin(nil)
	assert.NewAborting(t).Nil(s.execAdmin.Load(), "SetExecutorAdmin(nil) stored a pointer")
}

func TestSetUserAdminNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetUserAdmin(nil)
	assert.NewAborting(t).Nil(s.userAdmin.Load(), "SetUserAdmin(nil) stored a pointer")
}

func TestSetRawChildIONilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetRawChildIO(nil)
	assert.NewAborting(t).Nil(s.rawIO.Load(), "SetRawChildIO(nil) stored a pointer")
}

func TestSetExecutorSessionsNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetExecutorSessions(nil)
	assert.NewAborting(t).Nil(s.execSessions.Load(), "SetExecutorSessions(nil) stored a pointer")
}
