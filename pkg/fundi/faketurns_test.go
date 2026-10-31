package fundi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// TestFakeSenderDecodesAnthropicBodies pins the scripted-turn seam: the file
// format is unchanged (newline-separated Anthropic message bodies), decoded
// into native responses, and the native Sender both returns one whole and
// replays one as a stream.
func TestFakeSenderDecodesAnthropicBodies(t *testing.T) {
	c := assert.NewAborting(t)
	var lines []string
	for _, body := range []string{sampleResp, sampleEndTurn} {
		var compact bytes.Buffer
		c.NoError(json.Compact(&compact, []byte(body)), "compact scripted body")
		lines = append(lines, compact.String())
	}
	path := filepath.Join(t.TempDir(), "turns.ndjson")
	c.NoError(os.WriteFile(path, []byte("\n"+strings.Join(lines, "\n\n")+"\n"), 0o600), "write scripted turns")

	sender, err := LoadFakeSender(path)
	c.NoError(err, "LoadFakeSender")

	resp, err := sender.Send(context.Background(), nil, msg.EncodeOptions{})
	c.NoError(err, "Send")
	c.Eq(msg.StopToolUse, resp.StopReason, "stop reason")
	c.Eq("claude-x", resp.Model, "model")
	c.Require().Len(resp.Content, 2, "content")
	text, ok := resp.Content[0].(msg.Text)
	c.Require().True(ok, "content[0] = %T, want text", resp.Content[0])
	c.Eq("on it", text.Text, "text")
	tu, ok := resp.Content[1].(msg.ToolUse)
	c.Require().True(ok, "content[1] = %T, want tool_use", resp.Content[1])
	c.Eq("tu_1", tu.ID, "tool use id")
	c.Eq("bash", tu.Name, "tool name")
	var input map[string]any
	c.NoError(json.Unmarshal(tu.Input, &input), "tool input is not JSON")
	c.Eq("ls", input["command"], "tool input command")

	dec, err := sender.Stream(context.Background(), nil, msg.EncodeOptions{})
	c.NoError(err, "Stream")
	var kinds []string
	acc := msg.NewAccumulator()
	for {
		ev, derr := dec.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		c.NoError(derr, "Next")
		c.NoError(acc.Add(ev), "Add")
		switch ev.(type) {
		case msg.MessageStart:
			kinds = append(kinds, "start")
		case msg.BlockStart:
			kinds = append(kinds, "block-start")
		case msg.BlockStop:
			kinds = append(kinds, "block-stop")
		case msg.MessageDelta:
			kinds = append(kinds, "delta")
		default:
			kinds = append(kinds, "other")
		}
	}
	c.NoError(dec.Close(), "Close")
	c.EqDiff([]string{"start", "block-start", "block-stop", "delta"}, kinds, "stream event sequence")
	streamed := acc.Response()
	c.Eq(msg.StopEndTurn, streamed.StopReason, "streamed stop reason")
	c.Require().Len(streamed.Content, 1, "streamed content")
	streamedText, ok := streamed.Content[0].(msg.Text)
	c.Require().True(ok, "streamed content[0] = %T, want text", streamed.Content[0])
	c.Eq("done", streamedText.Text, "streamed text")

	_, err = sender.Send(context.Background(), nil, msg.EncodeOptions{})
	c.Error(err, "an exhausted sender must fail rather than replay its last turn")
}
