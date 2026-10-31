// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/tags"
	"go.graveland.dev/rafiki/pkg/users"
)

const (
	// defaultTagListLimit and maxTagListLimit bound ListTags: 0 on the wire
	// means the default, anything larger is clamped, and a negative limit is
	// the caller's mistake.
	defaultTagListLimit = 50
	maxTagListLimit     = 500
)

// TagCaller is who is asking: the connection's owner and, for a child
// credential, the child (from the credential, never the request).
//
// ChildID is what a child caller's authority is checked against — the tag
// store and the Controller decide what it may read, create and move, because
// this package holds no parent chain.
type TagCaller struct {
	Owner   users.Identity
	ChildID string
}

// TagInput is one TagRequest's payload, mapped onto the domain types.
//
// TagID empty means create; TagID and ExpectNodeID both set means move, and
// Name, Scope and Replace must then be unset. Scope is ScopeUnset only when
// the wire said TAG_SCOPE_UNSPECIFIED, which the store reads as lineage.
type TagInput struct {
	Name, ChildID, Cursor, Note, TagID, ExpectNodeID string
	Scope                                            tags.Scope
	Replace                                          bool
}

// TagManager is the narrow slice of the daemon needed to create, move,
// remove and list the caller's tags. It carries the caller explicitly rather
// than resolving an owner from the context, because a tag's authority is two
// facts — the owner and, for a child credential, the caller's position in the
// tree — and only the implementation can resolve both.
type TagManager interface {
	// Tag creates the tag (TagID empty) or moves it, returning the stored
	// row. Authority, name validation and the node the cursor names are the
	// implementation's: a create or move it may not perform must be a
	// *ControllerError, so ConnectErr gives the caller the right code.
	Tag(ctx context.Context, caller TagCaller, in TagInput) (tags.Tag, error)
	// Untag tombstones the tag's live row.
	Untag(ctx context.Context, caller TagCaller, tagID string) error
	// ListTags answers one tag's whole history when tagID is set, and
	// otherwise the live tags visible to caller whose name starts with
	// namePrefix, nearest scope first. limit is already resolved.
	ListTags(ctx context.Context, caller TagCaller, namePrefix string, limit int, tagID string) ([]tags.Tag, error)
}

// SetTagManager attaches the tags backend. A nil manager is refused rather
// than stored (see SetPymoduleManager): storing &m for a nil interface would
// defeat tagManager's Unavailable path and nil-panic the first handler call
// instead.
func (s *Server) SetTagManager(m TagManager) {
	if m == nil {
		return
	}
	s.tags.Store(&m)
}

func (s *Server) tagManager() (TagManager, error) {
	p := s.tags.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("tags not yet wired"))
	}
	return *p, nil
}

// tagCaller resolves who is asking: the connection's owner, resolved the way
// the preset verbs resolve it (the credential's user id and name, and
// deliberately not its admin bit — a tag is never daemon-wide state), and the
// calling child from the wired ChildScopeSource, never from a request field.
// A user credential and the unix socket's nil local trust both answer
// ChildID "".
func (s *Server) tagCaller(ctx context.Context) TagCaller {
	var c TagCaller
	if id := server.IdentityFromContext(ctx); id != nil {
		c.Owner = users.Identity{UserID: id.UserID, Username: id.Username}
	}
	if sc := s.childScope(ctx); sc != nil {
		c.ChildID = sc.ChildID()
	}
	return c
}

// Tag creates or moves a tag (childScoped at the gate: a child may tag the
// children it can already read, which the Controller's tag authority decides
// from caller.ChildID).
//
// ownerGuard: a non-admin user may tag only a child it owns. The guard runs
// before the store is touched, so a foreign child id never reaches it.
func (s *Server) Tag(
	ctx context.Context, req *connect.Request[rafikiv1.TagRequest],
) (*connect.Response[rafikiv1.TagResponse], error) {
	m, err := s.tagManager()
	if err != nil {
		return nil, err
	}
	scope, err := tagScopeFromWire(req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	in := TagInput{
		Name:         req.Msg.GetName(),
		ChildID:      req.Msg.GetChildId(),
		Cursor:       req.Msg.GetCursor(),
		Note:         req.Msg.GetNote(),
		TagID:        req.Msg.GetTagId(),
		ExpectNodeID: req.Msg.GetExpectNodeId(),
		Scope:        scope,
		Replace:      req.Msg.GetReplace(),
	}
	if g := s.ownerGuard(ctx); g != nil && in.ChildID != "" {
		if err := g.Authorize(in.ChildID); err != nil {
			return nil, err
		}
	}
	tag, err := m.Tag(ctx, s.tagCaller(ctx), in)
	if err != nil {
		return nil, unmappedErr(err, "rpc", "Tag", "name", in.Name, "child_id", in.ChildID)
	}
	return connect.NewResponse(&rafikiv1.TagResponse{Tag: tagRowToProto(tag)}), nil
}

// Untag tombstones a tag's live row. It needs no handler-layer owner guard:
// tags are keyed by the connection's owner in the store, so a foreign tag id
// is simply not found.
func (s *Server) Untag(
	ctx context.Context, req *connect.Request[rafikiv1.UntagRequest],
) (*connect.Response[rafikiv1.UntagResponse], error) {
	m, err := s.tagManager()
	if err != nil {
		return nil, err
	}
	tagID := req.Msg.GetTagId()
	if tagID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("tag_id is required"))
	}
	if err := m.Untag(ctx, s.tagCaller(ctx), tagID); err != nil {
		return nil, unmappedErr(err, "rpc", "Untag", "tag_id", tagID)
	}
	return connect.NewResponse(&rafikiv1.UntagResponse{}), nil
}

// ListTags answers one tag's history (tag_id set) or the live tags visible to
// the caller, nearest scope first. Like Untag it needs no owner guard: the
// store answers only the connection owner's tags.
func (s *Server) ListTags(
	ctx context.Context, req *connect.Request[rafikiv1.ListTagsRequest],
) (*connect.Response[rafikiv1.ListTagsResponse], error) {
	m, err := s.tagManager()
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	switch {
	case limit < 0:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("limit must not be negative"))
	case limit == 0:
		limit = defaultTagListLimit
	case limit > maxTagListLimit:
		limit = maxTagListLimit
	}
	rows, err := m.ListTags(ctx, s.tagCaller(ctx), req.Msg.GetNamePrefix(), limit, req.Msg.GetTagId())
	if err != nil {
		return nil, unmappedErr(err, "rpc", "ListTags", "name_prefix", req.Msg.GetNamePrefix())
	}
	out := make([]*rafikiv1.TagRow, 0, len(rows))
	for _, t := range rows {
		out = append(out, tagRowToProto(t))
	}
	return connect.NewResponse(&rafikiv1.ListTagsResponse{Rows: out}), nil
}

// tagScopeFromWire maps the request's scope. TAG_SCOPE_UNSPECIFIED is
// ScopeUnset, which only a create may carry (the store defaults it to
// lineage); any other value the schema does not define is refused rather than
// silently treated as a default.
func tagScopeFromWire(s rafikiv1.TagScope) (tags.Scope, error) {
	switch s {
	case rafikiv1.TagScope_TAG_SCOPE_UNSPECIFIED:
		return tags.ScopeUnset, nil
	case rafikiv1.TagScope_TAG_SCOPE_LINEAGE:
		return tags.ScopeLineage, nil
	case rafikiv1.TagScope_TAG_SCOPE_TREE:
		return tags.ScopeTree, nil
	case rafikiv1.TagScope_TAG_SCOPE_OWNER:
		return tags.ScopeOwner, nil
	}
	return tags.ScopeUnset, connect.NewError(connect.CodeInvalidArgument,
		errors.New("scope must be LINEAGE, TREE or OWNER"))
}

// tagScopeToWire is tagScopeFromWire's inverse. An unset scope (never stored)
// answers UNSPECIFIED rather than claiming lineage.
func tagScopeToWire(s tags.Scope) rafikiv1.TagScope {
	switch s {
	case tags.ScopeLineage:
		return rafikiv1.TagScope_TAG_SCOPE_LINEAGE
	case tags.ScopeTree:
		return rafikiv1.TagScope_TAG_SCOPE_TREE
	case tags.ScopeOwner:
		return rafikiv1.TagScope_TAG_SCOPE_OWNER
	}
	return rafikiv1.TagScope_TAG_SCOPE_UNSPECIFIED
}

// tagRowToProto maps one tag row onto the wire type. creator_is_user is the
// absence of a creator child; timestamps are omitted when unset, so a live tag
// carries neither superseded_at nor deleted_at.
func tagRowToProto(t tags.Tag) *rafikiv1.TagRow {
	out := &rafikiv1.TagRow{
		TagId:          t.TagID,
		Name:           t.Name,
		Scope:          tagScopeToWire(t.Scope),
		ScopeRoot:      t.ScopeRoot,
		ChildId:        t.SourceChildID,
		NodeId:         t.NodeID,
		Ordinal:        int32(t.Ordinal),
		CreatorChildId: t.CreatorChildID,
		CreatorIsUser:  t.CreatorChildID == "",
		ActorChildId:   t.ActorChildID,
		Note:           t.Note,
	}
	if !t.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(t.CreatedAt)
	}
	if t.SupersededAt != nil {
		out.SupersededAt = timestamppb.New(*t.SupersededAt)
	}
	if t.DeletedAt != nil {
		out.DeletedAt = timestamppb.New(*t.DeletedAt)
	}
	return out
}
