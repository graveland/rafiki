package childstore

import "strings"

// ExecutorSetAll is the executor-set entry meaning every enabled executor of
// the child's owner, resolved at call time. It is never combined with ids.
const ExecutorSetAll = "all"

// ResolveSetSpec interprets a stored executor set. A nil set is a row written
// before executor sets existed: its legacy selector, if any, still describes
// what it may address, and an empty legacy selector meant every executor of
// the owner.
func ResolveSetSpec(set []string, legacySelector string) (all bool, ids []string, selector string) {
	if set == nil {
		if strings.TrimSpace(legacySelector) == "" {
			return true, nil, ""
		}
		return false, nil, legacySelector
	}
	if len(set) == 1 && set[0] == ExecutorSetAll {
		return true, nil, ""
	}
	return false, set, ""
}

// SetExecutorSet replaces id's executor set. The caller persists the record.
func (s *Store) SetExecutorSet(id string, set []string) error {
	cp := copySet(set)
	return s.Update(id, func(sess *Session) { sess.ExecutorSet = cp })
}

// copySet copies a set and keeps nil and empty distinct: nil is a legacy row,
// empty is a child that addresses nothing.
func copySet(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}
