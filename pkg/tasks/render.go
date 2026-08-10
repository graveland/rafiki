package tasks

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// AssignHandles fills each Task's Handle with its dotted ordinal path
// ("2.1.7"). Handles are derived from stored ordinals, never from position
// in a list — a gap left by a dropped sibling stays visible, because closing
// it up would make an existing handle point at a different task.
func AssignHandles(all []Task) []Task {
	byID := make(map[string]Task, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}
	handle := func(t Task) string {
		var parts []string
		cur := t
		var ok bool
		for range 64 { // depth guard; a cycle must not hang a render
			parts = append([]string{strconv.Itoa(cur.Ordinal)}, parts...)
			if cur.ParentID == "" {
				break
			}
			cur, ok = byID[cur.ParentID]
			if !ok {
				break // parent outside this set: render the partial path
			}
		}
		return strings.Join(parts, ".")
	}
	out := make([]Task, len(all))
	copy(out, all)
	for i := range out {
		out[i].Handle = handle(out[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
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
