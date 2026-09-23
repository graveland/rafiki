// SPDX-License-Identifier: Apache-2.0

package presets

import (
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ToProto converts a Record to its wire form. A nil slice becomes a nil
// *StringList; a non-nil slice (even empty, meaning "none") becomes a
// non-nil *StringList — collapsing the two would turn "all tools" into
// "no tools". Times are RFC3339 UTC; a nil DeletedAt is "".
func ToProto(r Record) *rafikiv1.PresetRow {
	p := &rafikiv1.PresetRow{
		Version:            r.ID,
		Name:               r.Name,
		Description:        r.Description,
		Kind:               r.Kind,
		Provider:           r.Provider,
		Model:              r.Model,
		Thinking:           r.Thinking,
		Executor:           r.Executor,
		Labels:             r.Labels,
		Tools:              stringList(r.Tools),
		Skills:             stringList(r.Skills),
		McpServers:         stringList(r.MCPServers),
		ContextFiles:       copyPtr(r.ContextFiles),
		SystemPrompt:       r.SystemPrompt,
		AppendSystemPrompt: r.AppendSystemPrompt,
		MaxCost:            copyPtr(r.MaxCost),
		MaxDepth:           int32Ptr(r.MaxDepth),
		MaxChildren:        int32Ptr(r.MaxChildren),
		WrittenByChild:     r.WrittenByChild,
		CreatedAt:          r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.DeletedAt != nil {
		p.DeletedAt = r.DeletedAt.UTC().Format(time.RFC3339)
	}
	return p
}

// FromProto is the inverse of ToProto. A nil *StringList becomes a nil slice
// (the kind's default); a non-nil one becomes a NON-NIL slice, even when its
// Items are empty ("none" survives the wire). version maps to ID.
// Unparseable created_at/deleted_at are left zero/nil.
func FromProto(p *rafikiv1.PresetRow) Record {
	if p == nil {
		return Record{}
	}
	r := Record{
		ID:                 p.GetVersion(),
		Name:               p.GetName(),
		Description:        p.GetDescription(),
		Kind:               p.GetKind(),
		Provider:           p.GetProvider(),
		Model:              p.GetModel(),
		Thinking:           p.GetThinking(),
		Executor:           p.GetExecutor(),
		Labels:             p.GetLabels(),
		Tools:              items(p.GetTools()),
		Skills:             items(p.GetSkills()),
		MCPServers:         items(p.GetMcpServers()),
		ContextFiles:       copyPtr(p.ContextFiles),
		SystemPrompt:       p.GetSystemPrompt(),
		AppendSystemPrompt: p.GetAppendSystemPrompt(),
		MaxCost:            copyPtr(p.MaxCost),
		MaxDepth:           intPtr(p.MaxDepth),
		MaxChildren:        intPtr(p.MaxChildren),
		WrittenByChild:     p.GetWrittenByChild(),
	}
	if t, err := time.Parse(time.RFC3339, p.GetCreatedAt()); err == nil {
		r.CreatedAt = t
	}
	if s := p.GetDeletedAt(); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			r.DeletedAt = &t
		}
	}
	return r
}

// stringList wraps a tri-state slice for the wire: nil stays nil (absent =
// the kind's default), a non-nil slice — even empty ("none") — becomes a
// non-nil StringList.
func stringList(xs []string) *rafikiv1.StringList {
	if xs == nil {
		return nil
	}
	return &rafikiv1.StringList{Items: xs}
}

// items unwraps a wire StringList back into a tri-state slice: nil stays nil;
// a non-nil StringList becomes a NON-NIL slice even when Items is empty.
func items(l *rafikiv1.StringList) []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.Items))
	out = append(out, l.Items...)
	return out
}

// copyPtr returns a fresh pointer to *p so a Record and its wire form never
// alias, or nil when p is nil — a *bool/*float64 pointing at the zero value
// must survive as "set, zero", never collapse to unset.
func copyPtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func int32Ptr(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

func intPtr(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}
