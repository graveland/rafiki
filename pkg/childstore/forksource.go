// SPDX-License-Identifier: Apache-2.0

package childstore

// ForkSource is what forking or tagging a child needs to know about it, read
// from its durable row so a closed child answers too.
type ForkSource struct {
	ChildID     string
	OwnerUserID string
	Kind        string
	// ConversationID is the conversation the child's history lives in: for a
	// fundi child its own conversation, for a claude child the root thread's
	// conversation (child.conversation_id stays NULL for those).
	ConversationID string
	// ParentID and RootID come from the child's lineage labels; both are ""
	// at the top level.
	ParentID string
	RootID   string
	Closed   bool
}
