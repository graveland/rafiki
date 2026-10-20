package tasks

import (
	"context"
	"maps"
	"slices"
	"strings"
)

// Notifying wraps inner so every committed mutation is reported to notify.
//
// notify runs synchronously, after the inner call has returned without error
// and only when the mutation changed at least one row: a wrapper that reports
// a mutation the store refused, or one that changed nothing, hands its
// subscriber a row set that never existed. Reads pass straight through.
//
// A nil notify returns inner unchanged rather than a wrapper that does
// nothing, so a caller without a subscriber pays neither the indirection nor
// a new Store identity.
func Notifying(inner Store, notify func(Notice)) Store {
	if notify == nil {
		return inner
	}
	return &notifyingStore{Store: inner, notify: notify}
}

type notifyingStore struct {
	Store
	notify func(Notice)
}

func (s *notifyingStore) Add(ctx context.Context, convID, parentHandle string, items []NewTask) ([]Task, error) {
	rows, err := s.Store.Add(ctx, convID, parentHandle, items)
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	s.notify(Notice{Kind: NoticeAdded, ConversationID: convID, Tasks: rows})
	return rows, nil
}

func (s *notifyingStore) Update(ctx context.Context, convID string, changes []Change) ([]Task, error) {
	rows, err := s.Store.Update(ctx, convID, changes)
	if err != nil {
		return rows, err
	}
	named := make(map[string]bool, len(changes))
	for _, ch := range changes {
		named[ch.Handle] = true
	}
	var reported []Task
	for _, t := range rows {
		if named[t.Handle] {
			reported = append(reported, t)
		}
	}
	if len(reported) == 0 {
		return rows, nil
	}
	s.notify(Notice{Kind: NoticeUpdated, ConversationID: convID, Tasks: reported})
	return rows, nil
}

func (s *notifyingStore) Drop(ctx context.Context, convID, handle, reason string) ([]Task, error) {
	rows, err := s.Store.Drop(ctx, convID, handle, reason)
	if err != nil {
		return rows, err
	}
	reported := droppedFrom(rows, handle)
	if len(reported) == 0 {
		// A store that returns only live rows leaves the dropped subtree out
		// of Drop's return; ask again for it rather than report nothing.
		rows, err = s.List(ctx, ListFilter{ConversationID: convID, IncludeDropped: true})
		if err != nil {
			return nil, err
		}
		reported = droppedFrom(rows, handle)
	}
	if len(reported) == 0 {
		return rows, nil
	}
	s.notify(Notice{Kind: NoticeDropped, ConversationID: convID, Tasks: reported})
	return rows, nil
}

// droppedFrom keeps the rows a Drop of handle changed: the target and its
// subtree. The dot is part of the match so dropping "1" never claims "10".
func droppedFrom(rows []Task, handle string) []Task {
	var out []Task
	for _, t := range rows {
		if t.Handle == handle || strings.HasPrefix(t.Handle, handle+".") {
			out = append(out, t)
		}
	}
	return out
}

func (s *notifyingStore) Assign(ctx context.Context, convID, handle, assignee string) (Task, error) {
	t, err := s.Store.Assign(ctx, convID, handle, assignee)
	if err != nil {
		return t, err
	}
	s.notify(Notice{Kind: NoticeAssigned, ConversationID: convID, Tasks: []Task{t}})
	return t, nil
}

func (s *notifyingStore) OrphanAssigned(ctx context.Context, assignee string) ([]Task, error) {
	rows, err := s.Store.OrphanAssigned(ctx, assignee)
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	byConv := make(map[string][]Task)
	for _, t := range rows {
		byConv[t.ConversationID] = append(byConv[t.ConversationID], t)
	}
	for _, convID := range slices.Sorted(maps.Keys(byConv)) {
		s.notify(Notice{Kind: NoticeOrphaned, ConversationID: convID, Tasks: byConv[convID]})
	}
	return rows, nil
}
