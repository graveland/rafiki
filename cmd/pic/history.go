package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"git.graveland.dev/brent/pi-controller/client"
	"git.graveland.dev/brent/pi-controller/protocol"
)

// emitMachineFrame writes inner (a raw inner pi-event frame) for the machine-
// readable output modes: raw prints it verbatim as one JSONL line; otherwise it
// is pretty-printed. Using the inner event for both backfill and live keeps the
// two streams identical in shape. Falls back to the verbatim bytes if inner is
// not valid JSON.
func emitMachineFrame(w io.Writer, inner []byte, raw bool) {
	if raw {
		fmt.Fprintln(w, string(inner))
		return
	}
	var v any
	if err := json.Unmarshal(inner, &v); err != nil {
		fmt.Fprintln(w, string(inner))
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(w, string(inner))
		return
	}
	fmt.Fprintln(w, string(b))
}

// historyOpts configures runHistoryOut. Defaults differ by frontend:
// tail sets follow=true,tailN=20; logs sets follow=false,tailN=-1.
type historyOpts struct {
	follow   bool
	tailN    int // -1 = all, 0 = none, >0 = last N
	raw      bool
	profile  string
	include  []string
	exclude  []string
	verbose  bool
	mode     outputMode
	useColor bool
}

// innerEvent returns the inner pi-event bytes of a frame. Live frames are
// ctrl_event envelopes carrying the event under "event"; backfill frames from
// ctrl_get_recent are already the raw inner event. Returns the frame unchanged
// when it is not a ctrl_event envelope (e.g. lifecycle frames).
func innerEvent(frame []byte) []byte {
	var hdr struct {
		Type  string          `json:"type"`
		Event json.RawMessage `json:"event,omitempty"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return frame
	}
	if hdr.Type == protocol.TypeCtrlEvent && len(hdr.Event) > 0 {
		return hdr.Event
	}
	return frame
}

// fetchBackfill returns the last-N (per opts.tailN) out-stream event frames for
// childID via ctrl_get_recent. Frames are raw inner pi events. Returns nil when
// tailN == 0.
func fetchBackfill(ctx context.Context, c *client.Client, childID string, opts historyOpts) ([][]byte, error) {
	if opts.tailN == 0 {
		return nil, nil
	}
	limit := 0 // 0 = no limit (all) on the daemon side
	if opts.tailN > 0 {
		limit = opts.tailN
	}
	req := protocol.GetRecentRequest{
		Type:    protocol.TypeCtrlGetRecent,
		ChildID: childID,
		Limit:   limit,
		Include: opts.include,
		Exclude: opts.exclude,
	}
	resp, err := c.Request(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("ctrl_get_recent: %s", client.FormatError(resp))
	}
	var data protocol.GetRecentResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, err
	}
	out := make([][]byte, len(data.Events))
	for i, e := range data.Events {
		out[i] = []byte(e)
	}
	return out, nil
}

// runHistoryOut handles the default `out` event stream: optional backfill,
// then optional live follow. Used by both `tail` and `logs`.
//
// Backfill frames are raw inner pi events → rendered via renderPiEvent (or
// printed verbatim with --raw). Live frames are ctrl_event envelopes → rendered
// via render(). Dedup compares the inner bytes: any live frame whose inner event
// duplicates a backfilled frame (during the brief subscribe↔fetch overlap) is
// dropped, and the first non-duplicate closes the dedup window.
func runHistoryOut(ctx context.Context, c *client.Client, childID string, opts historyOpts) error {
	var events <-chan []byte
	var cancelSub func()
	if opts.follow {
		var err error
		events, cancelSub, err = c.Subscribe()
		if err != nil {
			return err
		}
		defer cancelSub()
		subReq := protocol.SubscribeRequest{Type: protocol.TypeCtrlSubscribe, ChildID: childID}
		if opts.profile != "" || len(opts.include) > 0 || len(opts.exclude) > 0 {
			subReq.Filter = &protocol.SubscribeFilter{Profile: opts.profile, Include: opts.include, Exclude: opts.exclude}
		}
		resp, err := c.Request(ctx, subReq)
		if err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("ctrl_subscribe: %s", client.FormatError(resp))
		}
	}

	backfill, err := fetchBackfill(ctx, c, childID, opts)
	if err != nil {
		return err
	}

	renderer := newTailRenderer(os.Stdout, opts.useColor, opts.mode, opts.verbose)

	// raw and --output json are the two machine-readable modes: both emit the
	// inner event for backfill and live so the two streams share one shape. Text
	// mode goes through the renderer.
	machine := opts.raw || opts.mode == outputJSON

	// Render backfill (raw inner pi events).
	// dedup assumes live inner bytes are byte-identical to ring bytes (true for the
	// pi identity provider over compacted JSON; claude re-marshals, so it
	// under-dedups harmlessly).
	seen := make(map[string]bool, len(backfill))
	for _, f := range backfill {
		seen[string(f)] = true
		if machine {
			emitMachineFrame(os.Stdout, f, opts.raw)
			continue
		}
		if err := renderer.renderPiEvent(f); err != nil {
			fmt.Fprintf(os.Stderr, "render error (child %s): %v\n", childID, err)
		}
	}

	if !opts.follow {
		return nil
	}

	dedupWindow := len(seen) > 0
	for {
		select {
		case frame, ok := <-events:
			if !ok {
				return nil
			}
			inner := innerEvent(frame)
			if dedupWindow {
				if seen[string(inner)] {
					delete(seen, string(inner))
					continue
				}
				dedupWindow = false // first non-duplicate closes the window
			}
			if machine {
				emitMachineFrame(os.Stdout, inner, opts.raw)
			} else if err := renderer.render(frame); err != nil {
				if errors.Is(err, errDaemonShutdown) {
					return nil
				}
				fmt.Fprintf(os.Stderr, "render error (child %s): %v\n", childID, err)
			}
			if isChildExited(frame, childID) {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}
