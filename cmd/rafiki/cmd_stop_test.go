package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestStopCmd_NoCloseFlagRemoved(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newStopCmd()
	c.Nil(cmd.Flags().Lookup("no-close"), "--no-close should be gone: stop never closes, so there is nothing to suppress")
	c.Nil(cmd.Flags().Lookup("no-forget"), "--no-forget should be gone: stop never closes, so there is nothing to suppress")
}

func TestStopCmd_KeepsKillAsAnAlias(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newStopCmd()
	c.Eq("stop", cmd.Name(), "Name()")
	var hasKill, hasK bool
	for _, a := range cmd.Aliases {
		switch a {
		case "kill":
			hasKill = true
		case "k":
			hasK = true
		}
	}
	c.True(hasKill, "`kill` must stay an alias: it is in muscle memory and in scripts")
	c.True(hasK, "`k` was kill's short alias and must survive the rename")
}

func TestRenderStopResults_JSON_CarriesError(t *testing.T) {
	c := assert.NewAborting(t)
	results := []stopTargetResult{
		{Arg: "c_ok", ChildID: "c_ok", Kill: &rafikiv1.KillResponse{ExitCode: int32Ptr(0)}},
		{Arg: "c_bad", Err: errors.New("child not found")},
	}
	var buf bytes.Buffer
	c.NoError(renderStopResults(&buf, results, outputJSON, false), "renderStopResults")
	var decoded struct {
		Results []stopResultJSON `json:"results"`
	}
	err := json.Unmarshal(buf.Bytes(), &decoded)
	c.NoError(err, "decode output: %v (raw=%s)", err, buf.String())
	c.Len(decoded.Results, 2, "results = %d, want 2", len(decoded.Results))
	if decoded.Results[0].ID != "c_ok" || decoded.Results[0].Error != "" {
		t.Errorf("results[0] = %+v, want id=c_ok error=\"\"", decoded.Results[0])
	}
	if decoded.Results[1].ID != "c_bad" || decoded.Results[1].Error != "child not found" {
		t.Errorf("results[1] = %+v, want id=c_bad error=\"child not found\"", decoded.Results[1])
	}
}

func TestRenderStopResults_Table_NoPanicOnNilExitCode(t *testing.T) {
	c := assert.NewCollecting(t)
	results := []stopTargetResult{
		{Arg: "c_bad", Err: errors.New("child not found")},
	}
	var buf bytes.Buffer
	c.Require().NoError(renderStopResults(&buf, results, outputTable, false), "renderStopResults")
	out := buf.String()
	c.False(!strings.Contains(out, "c_bad") || !strings.Contains(out, "child not found"), "table output missing id or error: %s", out)
}
