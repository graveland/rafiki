// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// errCodeByName lists every protocol.Err* constant in pkg/protocol/types.go by
// name and the Connect code the mapper must give it. The parser sweep below
// makes the pair exhaustive: a new Err* constant added to types.go fails here
// until it is listed here AND mapped in errCodeTable.
var errCodeByName = map[string]connect.Code{
	"ErrChildNotFound":      connect.CodeNotFound,
	"ErrChildExited":        connect.CodeFailedPrecondition,
	"ErrChildInGrace":       connect.CodeFailedPrecondition,
	"ErrChildShuttingDown":  connect.CodeFailedPrecondition,
	"ErrNotResumable":       connect.CodeFailedPrecondition,
	"ErrNotExited":          connect.CodeFailedPrecondition,
	"ErrSessionFileMissing": connect.CodeNotFound,
	"ErrBackpressure":       connect.CodeResourceExhausted,
	"ErrAtCapacity":         connect.CodeResourceExhausted,
	"ErrInvalidArgs":        connect.CodeInvalidArgument,
	"ErrSpawnFailed":        connect.CodeInternal,
	"ErrAuthRequired":       connect.CodeUnauthenticated,
	"ErrAuthInvalid":        connect.CodeUnauthenticated,
	"ErrNotFound":           connect.CodeNotFound,
	"ErrPermissionDenied":   connect.CodePermissionDenied,
	"ErrInternal":           connect.CodeInternal,
	"ErrNoAgentDB":          connect.CodeUnavailable,
	"ErrPayloadTooLarge":    connect.CodeInvalidArgument,
}

func TestConnectErrCoversEveryErrConstant(t *testing.T) {
	c := assert.NewCollecting(t)
	errValues := parseProtocolErrConsts(t)
	for name, value := range errValues {
		wantCode, listed := errCodeByName[name]
		if !listed {
			t.Errorf("protocol.%s (= %q) is not listed in errCodeByName; add it there and to errCodeTable in pkg/connectapi/errmap.go", name, value)
			continue
		}
		gotCode, mapped := errCodeTable[value]
		if !mapped {
			t.Errorf("protocol.%s = %q is missing from errCodeTable (want %v)", name, value, wantCode)
			continue
		}
		c.Eq(wantCode, gotCode, "errCodeTable[%q] = %v, want %v (protocol.%s)", value, gotCode, wantCode, name)
	}

	// The other direction: a table key that no longer names any Err* constant
	// in types.go is a stale entry.
	byValue := make(map[string]bool, len(errValues))
	for _, value := range errValues {
		byValue[value] = true
	}
	for value := range errCodeTable {
		c.False(!byValue[value], "errCodeTable key %q does not name any Err* constant in pkg/protocol/types.go", value)
	}
}

// parseProtocolErrConsts reads pkg/protocol/types.go with go/parser and returns
// every top-level const whose name starts with "Err", mapped to its string
// value. A constant without an explicit string literal fails the test: the
// sweep cannot read inherited const values, and every Err* constant today
// carries one.
func parseProtocolErrConsts(t *testing.T) map[string]string {
	t.Helper()
	c := assert.NewAborting(t)
	_, thisFile, _, ok := runtime.Caller(0)
	c.True(ok, "runtime.Caller could not locate this test file")
	typesPath := filepath.Join(filepath.Dir(thisFile), "..", "protocol", "types.go")
	file, err := parser.ParseFile(token.NewFileSet(), typesPath, nil, parser.SkipObjectResolution)
	c.NoError(err, "parse %s", typesPath)
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Err") {
					continue
				}
				if len(vs.Values) == 0 || i >= len(vs.Values) {
					t.Fatalf("protocol const %s has no explicit value; the sweep cannot read inherited const values — give it a string literal or extend parseProtocolErrConsts", name.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("protocol const %s is not a string literal; extend parseProtocolErrConsts", name.Name)
				}
				value, uerr := strconv.Unquote(lit.Value)
				c.NoError(uerr, "protocol const %s: unquote %s", name.Name, lit.Value)
				out[name.Name] = value
			}
		}
	}
	c.NotEmpty(out, "no Err* constants found in pkg/protocol/types.go — the parser sweep is broken")
	return out
}

func TestConnectErrRoundTripsReason(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("child_exited", rpcreason.Reason(ConnectErr(&ControllerError{Code: protocol.ErrChildExited})), "Reason(ConnectErr(ErrChildExited))")
	// The authored message is forwarded alongside the classification.
	authored := "child c_1 already exited"
	err := ConnectErr(&ControllerError{Code: protocol.ErrChildExited, Message: authored})
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.False(err == nil || !strings.Contains(err.Error(), authored), "err.Error() = %v, want containing the authored message", err)
}

// A non-ControllerError is infrastructure text this codebase did not author:
// ConnectErr redacts it to the fixed internal text and attaches no detail —
// a pgx failure cannot name the database through this surface. The cause is
// the caller's to log, never the peer's to read.
func TestConnectErrPlainErrorIsInternalNoReason(t *testing.T) {
	c := assert.NewCollecting(t)
	raw := "pgx: failed to connect to host=db.internal user=rafiki database=rafiki: connection refused"
	err := ConnectErr(errors.New(raw))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "code")
	got := rpcreason.Reason(err)
	c.Eq("", got, "Reason = %q, want \"\"", got)
	var ce *connect.Error
	c.False(!errors.As(err, &ce) || len(ce.Details()) != 0, "want a *connect.Error with no details, got %v", err)
	c.False(err == nil || !strings.Contains(err.Error(), internalErrText), "err.Error() = %v, want containing the fixed text %q", err, internalErrText)
	c.NotStrContains(err.Error(), "db.internal", "err.Error() = %v, want the raw cause redacted", err)
}

func TestConnectErrNilIsNil(t *testing.T) {
	assert.NewCollecting(t).NoError(ConnectErr(nil), "ConnectErr(nil)")
}

// The live-children-cap refusal is transient (the cap frees when a descendant
// settles), so it must classify as resource_exhausted like backpressure — not
// invalid_argument like the permanent limit refusals (zero cap, depth).
func TestErrCodeAtCapacityMapsToResourceExhausted(t *testing.T) {
	c := assert.NewAborting(t)
	err := ConnectErr(&ControllerError{Code: protocol.ErrAtCapacity, Message: "spawn refused: at its cap"})
	c.Eq(connect.CodeResourceExhausted, connect.CodeOf(err), "code")
	c.Eq(protocol.ErrAtCapacity, rpcreason.Reason(err), "Reason")
}

func TestConnectErrUnknownReasonStillAttached(t *testing.T) {
	c := assert.NewCollecting(t)
	err := ConnectErr(&ControllerError{Code: "some_future_code", Message: "explain"})
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "code")
	c.Eq("some_future_code", rpcreason.Reason(err), "Reason")
}
