package tasks

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// AssignHandles fills each Task's Handle with its dotted ordinal path
// ("2.1.7") and returns the list in tree order. Handles are derived from
// stored ordinals, never from position in a list — a gap left by a dropped
// sibling stays visible, because closing it up would make an existing handle
// point at a different task.
//
// Call this over the FULL set before filtering. A parent missing from the
// input yields a partial path, which is a handle that resolves to a
// different task. FilterTasks exists so callers never have to.
func AssignHandles(all []Task) []Task {
	byID := make(map[string]Task, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}
	// path returns the ordinal path root-first, e.g. [2, 1] for "2.1".
	path := func(t Task) []int {
		var parts []int
		cur := t
		var ok bool
		for range 64 { // depth guard; a cycle must not hang a render
			parts = append([]int{cur.Ordinal}, parts...)
			if cur.ParentID == "" {
				break
			}
			cur, ok = byID[cur.ParentID]
			if !ok {
				break // parent outside this set: render the partial path
			}
		}
		return parts
	}

	out := make([]Task, len(all))
	copy(out, all)
	paths := make([][]int, len(out))
	for i := range out {
		p := path(out[i])
		paths[i] = p
		strs := make([]string, len(p))
		for j, n := range p {
			strs[j] = strconv.Itoa(n)
		}
		out[i].Handle = strings.Join(strs, ".")
	}

	// Sort by conversation, then by the ordinal path component-wise. A
	// string sort would order 1, 10, 2 and put task 10 between 1.1 and 2,
	// which Render then indents as if it were a sibling of neither.
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sorted := make([]Task, 0, len(out))
	sort.SliceStable(idx, func(a, b int) bool {
		ta, tb := out[idx[a]], out[idx[b]]
		if ta.ConversationID != tb.ConversationID {
			return ta.ConversationID < tb.ConversationID
		}
		return comparePath(paths[idx[a]], paths[idx[b]]) < 0
	})
	for _, i := range idx {
		sorted = append(sorted, out[i])
	}
	return sorted
}

// comparePath orders two ordinal paths component-wise. A prefix sorts before
// its extensions, so a parent always precedes its children.
func comparePath(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// FilterTasks applies a ListFilter to an already-handled list. It is the ONLY
// filter implementation: both stores call it, which is what keeps the
// in-memory and Postgres paths from diverging on a predicate nobody tests
// twice.
//
// Order matters and is fixed: handles are assigned over the full set by
// AssignHandles, then rows are dropped here, then Limit truncates. Filtering
// before assigning handles renumbers survivors; limiting before sorting
// returns an arbitrary subset.
func FilterTasks(all []Task, f ListFilter) []Task {
	out := make([]Task, 0, len(all))
	for _, t := range all {
		if f.ConversationID != "" && t.ConversationID != f.ConversationID {
			continue
		}
		if f.Assignee != "" && t.Assignee != f.Assignee {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if !f.IncludeDropped && t.Status == StatusDropped {
			continue
		}
		skip := false
		for k, v := range f.Metadata {
			if t.Metadata[k] != v {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, t)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

func icon(s Status) string {
	switch s {
	case StatusPending:
		return "☐"
	case StatusInProgress:
		return "▣"
	case StatusCompleted:
		return "☑"
	case StatusBlocked:
		return "⊘"
	case StatusFailed:
		return "✗"
	case StatusOrphaned:
		return "⚠"
	case StatusDropped:
		return "–"
	}
	return "?"
}

// Render produces the text every task tool returns. The full list is echoed
// on every call deliberately: that is what keeps handles cheap, since the
// freshest ones are always in the most recent tool result and the model
// never carries them across turns.
func Render(all []Task, includeDropped bool) string {
	var sb strings.Builder
	counts := map[Status]int{}
	shown := 0
	for _, t := range all {
		counts[t.Status]++
		if t.Status == StatusDropped && !includeDropped {
			continue
		}
		shown++
	}
	fmt.Fprintf(&sb, "%d task(s):\n", shown)
	for _, t := range all {
		if t.Status == StatusDropped && !includeDropped {
			continue
		}
		depth := strings.Count(t.Handle, ".")
		fmt.Fprintf(&sb, "%s%s %s %s", strings.Repeat("  ", depth), t.Handle, icon(t.Status), t.Content)
		if t.Assignee != "" {
			fmt.Fprintf(&sb, " [@%s]", t.Assignee)
		}
		if t.Status == StatusDropped && t.DropReason != "" {
			fmt.Fprintf(&sb, " (dropped: %s)", t.DropReason)
		}
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "[%d pending, %d in_progress, %d blocked, %d completed, %d failed, %d orphaned, %d dropped]",
		counts[StatusPending], counts[StatusInProgress], counts[StatusBlocked],
		counts[StatusCompleted], counts[StatusFailed], counts[StatusOrphaned], counts[StatusDropped])
	return sb.String()
}
