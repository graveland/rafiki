// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(Render(&buf, modeVal{Name: "x", Count: 1}, ModeTable, tableVal))
	c.Eq("TABLE:x", buf.String(), "got")
}

func TestRenderModeJSONIndented(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(Render(&buf, modeVal{Name: "x", Count: 1}, ModeJSON, tableVal))
	got := buf.String()
	c.NotStrContains(got, "TABLE:", "table renderer ran in ModeJSON")
	c.StrContains(got, "\n  \"name\": \"x\"", "not indented")
}

func TestRenderModeJSONCompactIsOneLine(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(Render(&buf, modeVal{Name: "x", Count: 1}, ModeJSONCompact, tableVal))
	got := buf.String()
	c.Eq(`{"name":"x","count":1}`+"\n", got, "got")
}

func TestRenderModePropagatesTableError(t *testing.T) {
	boom := errors.New("boom")
	err := Render(io.Discard, modeVal{}, ModeTable, func(io.Writer, modeVal) error { return boom })
	assert.NewCollecting(t).ErrorIs(err, boom, "got")
}
