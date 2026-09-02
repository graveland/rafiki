package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestStopCmd_NoCloseFlagRemoved(t *testing.T) {
	cmd := newStopCmd()
	if cmd.Flags().Lookup("no-close") != nil {
		t.Error("--no-close should be gone: stop never closes, so there is nothing to suppress")
	}
	if cmd.Flags().Lookup("no-forget") != nil {
		t.Error("--no-forget should be gone: stop never closes, so there is nothing to suppress")
	}
}

func TestStopCmd_KeepsKillAsAnAlias(t *testing.T) {
	cmd := newStopCmd()
	if cmd.Name() != "stop" {
		t.Errorf("Name() = %q, want stop", cmd.Name())
	}
	var hasKill, hasK bool
	for _, a := range cmd.Aliases {
		switch a {
		case "kill":
			hasKill = true
		case "k":
			hasK = true
		}
	}
	if !hasKill {
		t.Error("`kill` must stay an alias: it is in muscle memory and in scripts")
	}
	if !hasK {
		t.Error("`k` was kill's short alias and must survive the rename")
	}
}

func TestRenderStopResults_JSON_CarriesError(t *testing.T) {
	results := []stopTargetResult{
		{Arg: "c_ok", ChildID: "c_ok", Kill: protocol.KillResponseData{ExitCode: intPtr(0)}},
		{Arg: "c_bad", Err: errors.New("child not found")},
	}
	var buf bytes.Buffer
	if err := renderStopResults(&buf, results, outputJSON, false); err != nil {
		t.Fatalf("renderStopResults: %v", err)
	}
	var decoded struct {
		Results []stopResultJSON `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode output: %v (raw=%s)", err, buf.String())
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(decoded.Results))
	}
	if decoded.Results[0].ID != "c_ok" || decoded.Results[0].Error != "" {
		t.Errorf("results[0] = %+v, want id=c_ok error=\"\"", decoded.Results[0])
	}
	if decoded.Results[1].ID != "c_bad" || decoded.Results[1].Error != "child not found" {
		t.Errorf("results[1] = %+v, want id=c_bad error=\"child not found\"", decoded.Results[1])
	}
}

func TestRenderStopResults_Table_NoPanicOnNilExitCode(t *testing.T) {
	results := []stopTargetResult{
		{Arg: "c_bad", Err: errors.New("child not found")},
	}
	var buf bytes.Buffer
	if err := renderStopResults(&buf, results, outputTable, false); err != nil {
		t.Fatalf("renderStopResults: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "c_bad") || !strings.Contains(out, "child not found") {
		t.Errorf("table output missing id or error: %s", out)
	}
}

func intPtr(i int) *int { return &i }
