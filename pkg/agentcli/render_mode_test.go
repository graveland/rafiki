// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type modeVal struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func tableVal(w io.Writer, v modeVal) error {
	_, err := io.WriteString(w, "TABLE:"+v.Name)
	return err
}

func TestRenderModeTableDelegates(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, modeVal{Name: "x", Count: 1}, ModeTable, tableVal); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "TABLE:x" {
		t.Errorf("got %q, want %q", got, "TABLE:x")
	}
}

func TestRenderModeJSONIndented(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, modeVal{Name: "x", Count: 1}, ModeJSON, tableVal); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "TABLE:") {
		t.Errorf("table renderer ran in ModeJSON: %q", got)
	}
	if !strings.Contains(got, "\n  \"name\": \"x\"") {
		t.Errorf("not indented: %q", got)
	}
}

func TestRenderModeJSONCompactIsOneLine(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, modeVal{Name: "x", Count: 1}, ModeJSONCompact, tableVal); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if want := `{"name":"x","count":1}` + "\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderModePropagatesTableError(t *testing.T) {
	boom := errors.New("boom")
	err := Render(io.Discard, modeVal{}, ModeTable, func(io.Writer, modeVal) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want %v", err, boom)
	}
}
