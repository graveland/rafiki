package main

import (
	"context"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// viewTranscript renders one agent_view read for spec.ChildID. A db-sourced
// child is pageable; a live or exited one gets the newest page, labelled, and
// refuses every cursor mode. rendered is GetRecent's Rendered flag for the
// non-db branches (the agent face passes true, the user face false).
//
// Callers authorize first: this function reads stored state and rendered text
// without asking who is calling.
//
// The source decision is dbSourced, the same helper GetRecent uses, so a child
// is never paged out of one source and rendered from another. A db-sourced read
// reads the transcript ITSELF and RETURNS a read error, where dbRecent logs and
// answers empty: an empty page would read as "the agent said nothing".
func (c *Controller) viewTranscript(spec tools.ViewSpec, rendered bool) (string, error) {
	snap, ok := c.st.Get(spec.ChildID)
	if !ok {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + spec.ChildID,
		}
	}

	// A page keeps today's rule (an absent or absurd limit is the default
	// page size); a find passes 0 through so renderFind applies
	// findDefaultHits, and only an over-large search is clamped.
	limit := spec.Limit
	if spec.Find != "" {
		if limit > viewMaxEntries {
			limit = viewMaxEntries
		}
	} else if limit <= 0 || limit > viewMaxEntries {
		limit = viewDefaultEntries
	}

	convID, fromDB := c.dbSourced(snap)
	if !fromDB {
		source := "exited"
		if _, alive := c.cm.Get(spec.ChildID); alive {
			source = "live"
		}
		if spec.Before != "" || spec.After != "" || spec.Entry != "" || spec.Find != "" {
			return "", fmt.Errorf(
				"%s is a %s child with no stored conversation, so it is not pageable yet — call agent_view without before/after/entry/find for its newest entries",
				spec.ChildID, source)
		}
		res, err := c.GetRecent(spec.ChildID, recentQuery{Rendered: rendered})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[%s child · newest entries only — not pageable]\n", source) +
			renderTranscript(res.Events, limit, viewMaxBytes), nil
	}

	var nodes []*conv.Node
	if c.convs != nil && convID != "" {
		loaded, err := c.convs.Transcript(context.Background(), convID)
		if err != nil {
			return "", fmt.Errorf("loading %s's conversation: %w", spec.ChildID, err)
		}
		nodes = loaded
	}
	frames, origins := fundi.DBToPiFramesWithOrigins(nodes)
	// renderEntries treats a mismatched origins slice as absent, which is right
	// for its nil-origins callers and wrong here: the entries would silently
	// carry no cursors, and every marker would print an empty before=/after=/
	// entry= (a cap with no way to continue). Refuse instead.
	if len(origins) != len(frames) {
		return "", fmt.Errorf("rendering %s's transcript: %d frames but %d origins — refusing to render uncursorable pages",
			spec.ChildID, len(frames), len(origins))
	}
	all := renderEntries(frames, origins)

	switch {
	case spec.Entry != "":
		return renderEntry(all, spec.Entry, viewMaxBytes)
	case spec.Find != "":
		start := 0
		if spec.After != "" {
			at, err := indexOfCursor(all, spec.After)
			if err != nil {
				return "", err
			}
			start = at + 1
		}
		return renderFind(all, spec.Find, start, limit, viewMaxBytes), nil
	}

	page := pageSpec{limit: limit, maxBytes: viewMaxBytes}
	switch {
	case spec.Before != "":
		at, err := indexOfCursor(all, spec.Before)
		if err != nil {
			return "", err
		}
		page.mode, page.at = pageBefore, at
	case spec.After != "":
		at, err := indexOfCursor(all, spec.After)
		if err != nil {
			return "", err
		}
		page.mode, page.at = pageAfter, at
	default:
		page.mode = pageNewest
	}
	return renderPage(all, page), nil
}
