// SPDX-License-Identifier: Apache-2.0

// Package modelalias holds the domain types and validation for per-owner
// model aliases. It is pgx-free so the client and connectapi may import it;
// pkg/modelaliasdb is the Postgres store.
package modelalias

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/routing"
)

// Alias is one row of conversations.model_alias: a slash-free name standing
// for a full model string (provider/id, optionally with a [routing spec]).
type Alias struct {
	Name      string
	Target    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	// ErrNotFound means the alias does not exist for the given owner.
	ErrNotFound = errors.New("model alias not found")
	// ErrInvalid means a name or target failed validation.
	ErrInvalid = errors.New("invalid model alias")
)

// Store is owner-scoped: every method takes the owner's user id ("" = the
// unattributed bucket, stored as SQL NULL) and touches only that owner's rows.
type Store interface {
	// Get returns name's alias, or ErrNotFound.
	Get(ctx context.Context, ownerUserID, name string) (Alias, error)
	// List returns every alias of the owner, ordered by name.
	List(ctx context.Context, ownerUserID string) ([]Alias, error)
	// Put validates (Validate) and creates or replaces name's alias,
	// returning the stored row. A replace keeps created_at and sets updated_at.
	Put(ctx context.Context, ownerUserID, name, target string) (Alias, error)
	// Delete removes name's alias, or returns ErrNotFound.
	Delete(ctx context.Context, ownerUserID, name string) error
}

// MaxNameLen bounds an alias name.
const MaxNameLen = 64

// nameRE is the shape of an alias name: lowercase alphanumeric, starting with
// an alphanumeric. A slash is impossible, which is what keeps a slash-free
// string on the control surface unambiguous (a slash-free name is an alias or
// an error; anything with a slash is a model id), and a bracket is impossible
// so a name can never be confused with a routing spec.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidateName: non-empty, <= MaxNameLen, matches nameRE. Errors wrap
// ErrInvalid and name the rule, e.g. `invalid model alias: name "Ds" must
// match ^[a-z0-9][a-z0-9._-]*$`.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalid)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%w: name %q is %d characters, must be at most %d", ErrInvalid, name, len(name), MaxNameLen)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalid, name, nameRE.String())
	}
	return nil
}

// ValidateTarget: routing.ParseModel(target) succeeds, and the parsed base has
// a "/" with a non-empty segment on each side of the FIRST slash. Requiring a
// provider segment is what rules out an alias chaining to another alias.
// Errors wrap ErrInvalid.
func ValidateTarget(target string) error {
	base, _, err := routing.ParseModel(target)
	if err != nil {
		return fmt.Errorf("%w: target %q: %v", ErrInvalid, target, err)
	}
	provider, id, ok := strings.Cut(base, "/")
	if !ok || provider == "" || id == "" {
		return fmt.Errorf("%w: target %q must be provider/model (a target cannot be another alias)", ErrInvalid, target)
	}
	return nil
}

// Validate is ValidateName then ValidateTarget.
func Validate(name, target string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return ValidateTarget(target)
}

// IsCandidate reports whether a model string could name an alias: its base
// (the part before any "[") is non-empty and contains no "/". Callers use it
// to skip the store for ordinary model ids.
//
// It deliberately does NOT call routing.ParseModel: a malformed bracket on a
// slashed id must stay the routing parser's error to report, not become an
// alias lookup.
func IsCandidate(model string) bool {
	base := model
	if i := strings.IndexByte(model, '['); i >= 0 {
		base = model[:i]
	}
	return base != "" && !strings.Contains(base, "/")
}
