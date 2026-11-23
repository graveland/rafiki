// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// TurnCompletion is everything the proxy holds when a provider response has
// arrived: the call's outcome and the request and response to record.
type TurnCompletion struct {
	TurnResult           // TurnID, CreatedAt, ConversationID, Model, StopReason, Upstream, ServedProvider, usage, LatencyMS
	Session       string // X-Rafiki-Session the thread was routed on
	PrevMessageID string // diagnostics.previous_message_id; "" for none
	OwnMessageID  string // the response's provider message id
	IsSubagent    bool   // cc_is_subagent
	ReqBody       []byte // the request body as sent upstream
	PrefixHash    string
	Canonical     []byte // the response's canonical JSON (its "content" array is stored)
}

// CompleteCapturedTurn records a whole captured call in ONE transaction: the
// request's missing nodes, the response node appended under them, and the turn
// row's outcome (status, usage, response_message_id, thread_id, request head,
// response ordinal, prefix). Either all of it lands or none of it does, so a
// turn row can never claim a reply the tree does not hold, nor the reverse.
//
// The turn row is keyed by (TurnID, CreatedAt) exactly as CompleteTurn is: a
// key that matches anything other than one row fails the call and rolls the
// appended nodes back with it.
//
// Thread classification is best-effort and runs before the transaction: a
// lookup failure logs and completes the turn with no thread rather than losing
// the call's record. The conversation's model is backfilled in the same
// transaction and only remembered once it commits, so a backfill that fails
// fails the call with the nodes and the turn row (a standalone backfill could
// only warn), and it writes no updated_at: the commit's own conversation update
// moves that whenever it stores a message.
//
// A call with no request messages appends no request node, so its reply hangs
// under the conversation's head as it stood before the call. When the reply is
// already stored, that node's parent is used instead, which keeps a re-issued
// completion the same node rather than a second copy of one reply.
func (s *CaptureStore) CompleteCapturedTurn(ctx context.Context, c TurnCompletion) error {
	msgs, err := requestMessages(c.ReqBody)
	if err != nil {
		return err
	}
	var resp struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(c.Canonical, &resp); err != nil {
		return fmt.Errorf("append response: unmarshal canonical: %w", err)
	}
	content := resp.Content
	if len(content) == 0 {
		content = json.RawMessage(`null`)
	}

	threadID, terr := s.threadResolver()(ctx, c.Session, c.ConversationID, c.TurnID, c.PrevMessageID, c.IsSubagent)
	if terr != nil {
		slog.Warn("capture: thread classification failed; completing with no thread",
			"conversation", c.ConversationID, "turn", c.TurnID, "error", terr)
		threadID = ""
	}

	// A request carrying no messages appends nothing, so its reply hangs under the
	// conversation's head, and that head is read ONCE here rather than per attempt
	// (an attempt's commit moves it). When the reply is already stored, a re-issued
	// completion hangs it under that node's parent instead, so the replay is the
	// same node rather than a second copy of one reply.
	var emptyParent string
	if len(msgs) == 0 {
		h := s.handle(c.ConversationID)
		var stored bool
		if c.OwnMessageID != "" {
			n, ok, err := h.NodeByProviderMessageID(ctx, c.OwnMessageID)
			if err != nil {
				return fmt.Errorf("attach: resolve the stored reply: %w", err)
			}
			if ok {
				emptyParent, stored = n.ParentID, true
			}
		}
		if !stored {
			head, err := requestHead(ctx, h, c.ConversationID)
			if err != nil {
				return err
			}
			emptyParent = head
		}
	}

	err = retryDB(ctx, "completeCapturedTurn", func(ctx context.Context) error {
		h := s.handle(c.ConversationID)
		plan := conv.Append{ParentID: emptyParent}
		if len(msgs) > 0 {
			var err error
			plan, err = s.planRequest(ctx, h, c.ConversationID, c.CreatedAt, msgs, c.PrevMessageID)
			if err != nil {
				return fmt.Errorf("decompose: %w", err)
			}
		}
		changed, err := s.prefixChanged(ctx, c.ConversationID, c.CreatedAt, c.PrefixHash)
		if err != nil {
			return err
		}
		envelope, breakpoints, err := splitEnvelopeAndBreakpoints(c.ReqBody)
		if err != nil {
			return fmt.Errorf("store turn prefix: %w", err)
		}
		var idx []int
		if err := json.Unmarshal(breakpoints, &idx); err != nil {
			return fmt.Errorf("store turn prefix: read breakpoints: %w", err)
		}
		bps, err := json.Marshal(idx)
		if err != nil {
			return fmt.Errorf("store turn prefix: marshal breakpoints: %w", err)
		}

		in, out := c.InputTokens, c.OutputTokens
		reqNodes := len(plan.Nodes)
		nodes := make([]conv.NewNode, 0, reqNodes+1)
		nodes = append(nodes, plan.Nodes...)
		nodes = append(nodes, conv.NewNode{
			Msg:               msg.Message{Role: msg.Assistant},
			Content:           conv.StorableJSON(content),
			InputTokens:       &in,
			OutputTokens:      &out,
			StopReason:        c.StopReason,
			ProviderMessageID: c.OwnMessageID,
			Origin:            conv.OriginClaudeCode,
		})
		plan.Nodes = nodes

		plan.Finalize = func(ctx context.Context, x conv.TxExec, res conv.Result) error {
			// res holds one entry per input node, in order, held ones included,
			// so the response is the entry after the request's nodes and the
			// head is the last request node (or the plan's parent when the
			// request appended nothing).
			head := plan.ParentID
			if reqNodes > 0 {
				head = res.Nodes[reqNodes-1].ID
			}
			if head == "" {
				return fmt.Errorf("attach: append produced no head node")
			}
			respOrd := res.Nodes[reqNodes].Ordinal
			n, err := x.Exec(ctx, `UPDATE conversations.conversation_turn SET
        status='complete', response=$3, stop_reason=$4, upstream=$5,
        input_tokens=$6, output_tokens=$7, cache_read_tokens=$8, cache_creation_tokens=$9, latency_ms=$10,
        model=COALESCE(NULLIF($11,''), model), served_provider=$12,
        response_message_id=NULLIF($13,''), thread_id=$14::uuid,
        request_head_id=$15::uuid, response_ordinal=$16,
        prefix_content=CASE WHEN $17 THEN $18::jsonb ELSE prefix_content END, cache_breakpoints=$19
    WHERE id=$1::uuid AND created_at=$2`,
				c.TurnID, c.CreatedAt, nullifyBytes(conv.StorableJSON(c.Response)), nullify(c.StopReason), nullify(c.Upstream),
				c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheCreationTokens, c.LatencyMS, c.Model,
				nullify(c.ServedProvider), c.OwnMessageID, nullUUID(threadID), head, respOrd, changed,
				conv.StorableJSON(envelope), bps)
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("complete-turn: expected 1 row, updated %d (stale/skewed key)", n)
			}
			// First-seen-wins backfill of the conversation's model, skipped once
			// the store knows the column is set. No now() and no increment:
			// an ambiguous commit re-runs this hook, so every statement here
			// must be a constant of the call.
			if c.Model != "" && !s.memo.modelKnown(c.ConversationID) {
				if _, err := x.Exec(ctx,
					`UPDATE conversations.conversation SET model = $2 WHERE id = $1::uuid AND model IS NULL`,
					c.ConversationID, c.Model); err != nil {
					return err
				}
			}
			return nil
		}

		if _, err := h.Append(ctx, plan); err != nil {
			return fmt.Errorf("attach: append chain: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if c.Model != "" {
		s.memo.markModel(c.ConversationID)
	}
	return nil
}

// FailCompletedTurn records a captured call that produced no usable response
// body: the turn becomes an error carrying whatever the call learned (usage,
// latency, upstream, model). Nothing is appended to the tree.
func (s *CaptureStore) FailCompletedTurn(ctx context.Context, r TurnResult, errMsg string) error {
	return retryDB(ctx, "failCompletedTurn", func(ctx context.Context) error {
		tag, err := s.pool.Exec(ctx, `UPDATE conversations.conversation_turn SET
        status='error', error=$3, stop_reason=$4, upstream=$5,
        input_tokens=$6, output_tokens=$7, cache_read_tokens=$8, cache_creation_tokens=$9, latency_ms=$10,
        model=COALESCE(NULLIF($11,''), model), served_provider=$12
    WHERE id=$1::uuid AND created_at=$2`,
			r.TurnID, r.CreatedAt, errMsg, nullify(r.StopReason), nullify(r.Upstream),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.LatencyMS, r.Model,
			nullify(r.ServedProvider))
		if err != nil {
			return err
		}
		if n := tag.RowsAffected(); n != 1 {
			return fmt.Errorf("fail-completed-turn: expected 1 row, updated %d (stale/skewed key)", n)
		}
		return nil
	})
}
