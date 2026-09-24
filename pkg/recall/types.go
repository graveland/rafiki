// Package recall indexes captured conversations and curated memories for
// search: extraction, windowing, hybrid ranking, and the background indexer.
package recall

import (
	"fmt"
	"time"
)

// Scope limits what a conversation-derived read may return. The zero value
// admits nothing: callers must either ask for everything (daemon trust level)
// or name an owner.
type Scope struct {
	All         bool
	OwnerUserID string
}

// Valid reports whether s admits anything. Scope{} and Scope{OwnerUserID: ""} admit nothing.
func (s Scope) Valid() bool { return s.All || s.OwnerUserID != "" }

// Source identifies which recall index a hit came from.
type Source string

const (
	SourceMemory  Source = "memory"
	SourceSummary Source = "summary"
	SourceWindow  Source = "window"
)

// Hit is one search result from any source, before or after fusion. Store
// implementations fill ID, Source, Snippet, When, the descriptive fields and
// Rank; Search fills Score.
type Hit struct {
	ID               string
	Source           Source
	Snippet          string
	When             time.Time
	ConversationID   string
	ConversationName string
	Repo             string
	Kind             string
	OrdinalFrom      int
	OrdinalTo        int
	Path             string
	Name             string
	Title            string
	Rank             int     // 1-based rank within its source list (set by Store)
	Score            float64 // fused RRF score (set by Search)
}

// SearchQuery carries one recall search: conversation-derived sources read
// under Scope, memories under MemoryOwner.
type SearchQuery struct {
	Scope       Scope  // conversation-derived sources
	MemoryOwner string // memory source; "" skips memories
	Text        string
	Sources     []Source // empty = all three
	Under       string   // ltree path prefix for memories; "" = all
	Repo        string   // basename match for conversation sources; "" = all
	Since       *time.Time
	Until       *time.Time
	Limit       int // per-source fetch size
}

// HitID renders a source row id as a hit id: "m:"+id, "s:"+id or "w:"+id.
func HitID(src Source, uuid string) string {
	switch src {
	case SourceMemory:
		return "m:" + uuid
	case SourceSummary:
		return "s:" + uuid
	case SourceWindow:
		return "w:" + uuid
	}
	return string(src) + ":" + uuid
}

// ParseHitID splits a hit id into its source and row id. An unknown prefix or
// an empty row id is an error.
func ParseHitID(id string) (Source, string, error) {
	var src Source
	if len(id) >= 2 && id[1] == ':' {
		switch id[0] {
		case 'm':
			src = SourceMemory
		case 's':
			src = SourceSummary
		case 'w':
			src = SourceWindow
		}
	}
	if src == "" {
		return "", "", fmt.Errorf("recall: bad hit id %q", id)
	}
	rest := id[2:]
	if rest == "" {
		return "", "", fmt.Errorf("recall: bad hit id %q", id)
	}
	return src, rest, nil
}
