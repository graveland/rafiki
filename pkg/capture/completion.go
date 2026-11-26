// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/anthropicreq"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// TurnCompletion is everything the proxy holds when a provider response has
// arrived: the call's outcome and the request and response to record.
type TurnCompletion struct {
	TurnResult                          // TurnID, CreatedAt, ConversationID, Model, StopReason, Upstream, ServedProvider, usage, LatencyMS
	Session       string                // X-Rafiki-Session the thread was routed on
	PrevMessageID string                // diagnostics.previous_message_id; "" for none
	OwnMessageID  string                // the response's provider message id
	IsSubagent    bool                  // cc_is_subagent
	Req           *anthropicreq.Request // the request as sent upstream, decoded once
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
//
// A turn that already holds a final state is left alone: a re-issued completion
// returns success without appending, so it cannot move the head back onto a
// reply that later turns have moved past.
func (s *CaptureStore) CompleteCapturedTurn(ctx context.Context, c TurnCompletion) error {
	msgs, err := requestMessages(c.Req)
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

	threadID, chainKey := s.classifyThread(ctx, c)
	// Read once, before the retry: the backfill is skipped when the store already
	// knows the column is set, which keeps the hook a function of the call.
	needModel := c.Model != "" && !s.memo.modelKnown(c.ConversationID)

	// A request carrying no messages appends nothing, so its reply hangs under the
	// conversation's head, read ONCE (an attempt's commit moves it). When the reply
	// is already stored, that node's parent anchors the replay instead.
	var emptyParent string
	if len(msgs) == 0 {
		err := retryDB(ctx, "resolveEmptyParent", func(ctx context.Context) error {
			h := s.handle(c.ConversationID)
			if c.OwnMessageID != "" {
				n, ok, err := h.NodeByProviderMessageID(ctx, c.OwnMessageID)
				if err != nil {
					return fmt.Errorf("attach: resolve the stored reply: %w", err)
				}
				if ok {
					emptyParent = n.ParentID
					return nil
				}
			}
			head, err := requestHead(ctx, h, c.ConversationID)
			if err != nil {
				return err
			}
			emptyParent = head
			return nil
		})
		if err != nil {
			return err
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
		envelope, breakpoints, err := splitEnvelopeAndBreakpoints(c.Req)
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
        status='complete', error=NULL, response=$3, stop_reason=$4, upstream=$5,
        input_tokens=$6, output_tokens=$7, cache_read_tokens=$8, cache_creation_tokens=$9, latency_ms=$10,
        model=COALESCE(NULLIF($11,''), model), served_provider=$12,
        response_message_id=NULLIF($13,''), thread_id=$14::uuid,
        request_head_id=$15::uuid, response_ordinal=$16,
        prefix_content=CASE WHEN $17 THEN $18::jsonb ELSE prefix_content END, cache_breakpoints=$19
    WHERE id=$1::uuid AND created_at=$2 AND status <> 'complete'`,
				c.TurnID, c.CreatedAt, nullifyBytes(conv.StorableJSON(c.Response)), nullify(c.StopReason), nullify(c.Upstream),
				c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheCreationTokens, c.LatencyMS, c.Model,
				nullify(c.ServedProvider), chainKey, nullUUID(threadID), head, respOrd, changed,
				conv.StorableJSON(envelope), bps)
			if err != nil {
				return err
			}
			if n != 1 {
				// Zero rows is either a key that matches nothing or a turn that already
				// holds a final state. Refuse the commit either way; the caller reads the
				// row to tell them apart, so a re-issued completion stores nothing.
				return errTurnNotPending
			}
			// First-seen-wins backfill of the conversation's model, skipped once
			// the store knows the column is set. No now() and no increment:
			// an ambiguous commit re-runs this hook, so every statement here
			// must be a constant of the call.
			if needModel {
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
		if errors.Is(err, errTurnNotPending) {
			done, derr := s.turnCompleted(ctx, c.TurnID, c.CreatedAt)
			if derr != nil {
				return derr
			}
			if done {
				// The call is already recorded: a re-issued completion stores nothing
				// and must not move the head back onto its own reply.
				if c.Model != "" {
					s.memo.markModel(c.ConversationID)
				}
				return nil
			}
			return fmt.Errorf("complete-turn: expected 1 row, updated 0 (stale/skewed key)")
		}
		return err
	}
	if c.Model != "" {
		s.memo.markModel(c.ConversationID)
	}
	return nil
}

// errTurnNotPending reports that the turn's own UPDATE matched no pending row:
// either the key names nothing or the turn is already final. The caller reads
// the row to tell those apart.
var errTurnNotPending = errors.New("complete-turn: turn is not pending")

// classifyThread resolves the turn's thread, best-effort: a lookup failure logs
// and completes the turn with no thread rather than losing the call's record.
//
// The second return value is the chain key later turns resolve this one by. It
// is empty when classification failed, because a NULL thread_id reads as "the
// main thread" and would silently route a subagent's later turns onto the
// session's main conversation; with no chain key the failure breaks the chain
// instead, so the next turn re-founds its thread.
func (s *CaptureStore) classifyThread(ctx context.Context, c TurnCompletion) (threadID, chainKey string) {
	resolve := s.threadResolver()
	var id string
	err := retryDB(ctx, "resolveThread", func(ctx context.Context) error {
		var rerr error
		id, rerr = resolve(ctx, c.Session, c.ConversationID, c.TurnID, c.PrevMessageID, c.IsSubagent)
		return rerr
	})
	if err != nil {
		slog.Warn("capture: thread classification failed; completing with no thread and no chain key",
			"conversation", c.ConversationID, "turn", c.TurnID, "error", err)
		return "", ""
	}
	return id, c.OwnMessageID
}

// turnCompleted reports whether the turn already holds the call's final state.
// A key that names no row is not an error: the completion's own UPDATE reports
// that case.
func (s *CaptureStore) turnCompleted(ctx context.Context, turnID string, createdAt time.Time) (bool, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM conversations.conversation_turn WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("complete-turn: read turn status: %w", err)
	}
	return status == "complete", nil
}

// FailCompletedTurn records a captured call that produced no usable response
// body: the turn becomes an error carrying whatever the call learned (usage,
// latency, upstream, model). Nothing is appended to the tree.
//
// A turn that already completed is left alone: the commit landed and the caller
// saw an error on the way back, so the call IS recorded and downgrading it would
// produce the very disagreement this rewrite exists to prevent.
func (s *CaptureStore) FailCompletedTurn(ctx context.Context, r TurnResult, errMsg string) error {
	return retryDB(ctx, "failCompletedTurn", func(ctx context.Context) error {
		tag, err := s.pool.Exec(ctx, `UPDATE conversations.conversation_turn SET
        status='error', error=$3, stop_reason=$4, upstream=$5,
        input_tokens=$6, output_tokens=$7, cache_read_tokens=$8, cache_creation_tokens=$9, latency_ms=$10,
        model=COALESCE(NULLIF($11,''), model), served_provider=$12
    WHERE id=$1::uuid AND created_at=$2 AND status <> 'complete'`,
			r.TurnID, r.CreatedAt, errMsg, nullify(r.StopReason), nullify(r.Upstream),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.LatencyMS, r.Model,
			nullify(r.ServedProvider))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		done, derr := s.turnCompleted(ctx, r.TurnID, r.CreatedAt)
		if derr != nil {
			return derr
		}
		if done {
			return nil
		}
		return fmt.Errorf("fail-completed-turn: expected 1 row, updated 0 (stale/skewed key)")
	})
}
