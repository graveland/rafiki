// SPDX-License-Identifier: Apache-2.0

// Package session holds one child's conversation as a pure state machine.
//
// It takes events and nothing else: no bubbletea, no network, no terminal.
// That purity is the point -- rendering and transport are the shell's problem,
// and a conversation you can drive from a slice of events is a conversation you
// can test.
package session

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"  // registered for DecodeConfig
	_ "image/jpeg" // registered for DecodeConfig
	_ "image/png"  // registered for DecodeConfig
	"slices"
	"strconv"
	"strings"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Kind identifies what kind of conversational unit a Block is.
type Kind int

const (
	KindSystem Kind = iota
	KindUser
	KindAssistant
	KindPendingUser
	// KindScriptOutput is one chunk of a script child's stdout or stderr. Its
	// Stream field says which; Text is the verbatim output.
	KindScriptOutput
	// KindInjected is text the child's own harness put into the conversation
	// rather than the user -- a heartbeat nudge, an environment update, a
	// file-changed notice. It covers both shapes on the wire: a system-role
	// message, and an injected block of a user-role message. It is real history,
	// so it stays in the model, but it is not conversation: the cockpit hides
	// these unless asked, and it is a kind of its own rather than KindSystem
	// because KindSystem (errors, compaction boundaries) is always worth seeing.
	KindInjected
)

// ToolCall is one tool invocation inside an assistant turn.
type ToolCall struct {
	ID     string
	Name   string
	Input  string
	Result string
	// Images holds the image blocks the tool result carried, in order.
	Images []*rafikiv1.ImageBlock
	// HasResult records that a tool_result actually arrived, which is NOT the
	// same as Result being non-empty: a tool can legitimately return nothing.
	// Without it a call that never produced a result at all — interrupted, or
	// its turn cut short — is indistinguishable from one that succeeded
	// silently, and the transcript claims a ✓ for work that never finished.
	// There are real instances of this: a production database here holds 38
	// bash calls with no matching tool_result.
	HasResult  bool
	IsError    bool
	Running    bool
	DurationMs int64
}

// Block is one renderable unit in the transcript.
type Block struct {
	Kind    Kind
	At      time.Time
	Text    string // user/pending text or assistant plain-text fallback
	Content []*rafikiv1.ContentBlock
	// Images holds the block's own top-level image blocks, in order.
	Images     []*rafikiv1.ImageBlock
	ThinkText  string // accumulated thinking
	ToolCalls  []ToolCall
	StopReason string
	Final      bool // true when the turn has ended
	// Stream marks a KindScriptOutput block's stream ("stdout"/"stderr").
	Stream string
	// InjectedKind says WHAT was injected on a KindInjected block (pkg/inject's
	// vocabulary: "jobs", "daemon-restart", "subagents", ...), so the renderer
	// can decorate one kind differently from another. Empty when the wire
	// named none — an older daemon, or a Claude Code reminder.
	InjectedKind string
	// MessageID is the inbox row id this user block delivered, when the daemon
	// linked a captured message to the send that carried it. It identifies; it
	// never changes how the block is drawn. Empty otherwise.
	MessageID string
	// Provisional marks a user block standing in for an accepted send whose
	// captured message has not arrived. It holds that send's PLACE in the
	// transcript — above the reply already streaming — until the linked
	// message replaces it or the turn ends.
	Provisional bool
}

// Fingerprint returns a cheap content hash for cache-invalidation.
func (b Block) Fingerprint() string {
	var sb strings.Builder
	sb.WriteString(b.Text)
	sb.WriteString(b.ThinkText)
	for _, tc := range b.ToolCalls {
		sb.WriteString(tc.ID)
		sb.WriteString(tc.Name)
		sb.WriteString(tc.Input)
		sb.WriteString(tc.Result)
		writeImages(&sb, tc.Images)
		// HasResult, IsError and DurationMs all change what is drawn without
		// changing Result -- a tool that succeeds returning nothing moves only
		// HasResult, and the whole point of that flag is that it is not the
		// same question as an empty string.
		if tc.HasResult {
			sb.WriteString("has")
		}
		if tc.IsError {
			sb.WriteString("err")
		}
		sb.WriteString(strconv.FormatInt(tc.DurationMs, 10))
		if tc.Running {
			sb.WriteString("running")
		}
	}
	writeImages(&sb, b.Images)
	sb.WriteString(b.StopReason)
	sb.WriteString(b.InjectedKind)
	if b.Stream != "" {
		sb.WriteString(b.Stream)
	}
	if b.Final {
		sb.WriteString("final")
	}
	if b.Provisional {
		sb.WriteString("provisional")
	}
	return sb.String()
}

// LiveFingerprint hashes every block that can still change.
//
// The renderer re-renders the whole region from finalized to the end, so the
// invalidation key has to cover the whole region too. Fingerprinting only the
// last block was correct while exactly one block could be unfinalized; it
// became a stale-render hole the moment an assistant block stayed live until
// its tool calls resolved.
func LiveFingerprint(blocks []Block, finalized int) string {
	if finalized < 0 {
		finalized = 0
	}
	if finalized >= len(blocks) {
		return ""
	}
	var sb strings.Builder
	for i := finalized; i < len(blocks); i++ {
		sb.WriteString(blocks[i].Fingerprint())
		sb.WriteByte(0x1e)
	}
	return sb.String()
}

// Settled reports whether b can never change again.
//
// This is the whole meaning of Session.Finalized, and getting it wrong is
// silent: renderer.Lines caches every block below the watermark exactly once
// and never looks at it again, so a block finalized too early is frozen on
// screen showing whatever was true at that instant. An assistant block's tool
// calls resolve AFTER the message announcing them arrives, which is why
// Final alone is not enough.
func (b Block) Settled() bool {
	// A provisional block is the one thing here that is KNOWN to change: its
	// linked capture replaces it, or the turn ending drops it. Caching it below
	// the watermark would freeze the send's "delivered" decoration on screen
	// with nothing left to invalidate it.
	if b.Provisional {
		return false
	}
	if b.Kind != KindAssistant {
		return true
	}
	if !b.Final {
		return false
	}
	for _, tc := range b.ToolCalls {
		if !tc.HasResult {
			return false
		}
	}
	return true
}

// Session is one child's conversation.
type Session struct {
	ChildID string
	Blocks  []Block
	// Finalized is the index into Blocks after which blocks may still change.
	Finalized int
	// Cursor is the highest ordinal delivered to this session, and HasCursor
	// says whether any ordinal has been seen at all. They are separate because
	// 0 is a legal ordinal -- a child's first event is ordinal 0, so a zero
	// Cursor cannot mean "nothing yet".
	Cursor    int32
	HasCursor bool
	// Status is the last agent_status state seen.
	Status string
	// linked is every inbox row id the daemon has linked to its send in this
	// transcript. It outlives the block that carried the id, because a captured
	// message can be linked and yet contribute no block of its own — a
	// results-only or injected-only row is dropped rather than drawn — and a
	// receipt that joins afterwards must not insert a block for it again.
	linked map[string]struct{}
}

// New returns an empty session for one child.
func New(childID string) *Session {
	return &Session{ChildID: childID}
}

// markLinked records that messageID's send has been linked to a captured
// message.
func (s *Session) markLinked(messageID string) {
	if messageID == "" {
		return
	}
	if s.linked == nil {
		s.linked = map[string]struct{}{}
	}
	s.linked[messageID] = struct{}{}
}

// InsertProvisional puts an accepted send into the transcript at the position
// its captured message will occupy.
//
// The send is already on the wire — its receipt says so — and the reply it
// prompted may already be streaming. Appending the canonical message when
// capture finally supplies it would land it BELOW that reply; this block holds
// the place instead, and applyUserMessage replaces it in place.
//
// It does nothing when any block already carries the same message id, or when
// this session has already linked that id: the linked capture may have arrived
// first, and a second block would be a duplicate row. An empty id names no send
// and can never be linked, so it inserts nothing.
func (s *Session) InsertProvisional(messageID, text string) {
	if messageID == "" {
		return
	}
	if _, linked := s.linked[messageID]; linked {
		return
	}
	if slices.ContainsFunc(s.Blocks, func(b Block) bool { return b.MessageID == messageID }) {
		return
	}
	if !s.insertBeforePlaceholder([]Block{{
		Kind:        KindUser,
		At:          time.Now(),
		Text:        text,
		MessageID:   messageID,
		Provisional: true,
		Final:       true,
	}}) {
		s.recomputeFinalized()
	}
}

// DropProvisional removes every provisional block, returning their message ids.
//
// This is what ends a send whose captured message never came: the turn that
// answered it is over, so the block it stood in for will not arrive in this
// transcript.
func (s *Session) DropProvisional() []string {
	var ids []string
	first := -1
	kept := s.Blocks[:0]
	for i, b := range s.Blocks {
		if !b.Provisional {
			kept = append(kept, b)
			continue
		}
		if first < 0 {
			first = i
		}
		ids = append(ids, b.MessageID)
	}
	if first < 0 {
		return nil
	}
	s.Blocks = kept
	// A turn end settles everything, watermark included, so a block removed
	// from below it shifts the blocks above down. Reopen the watermark there
	// rather than leaving it naming a block that has moved.
	if first < s.Finalized {
		s.Finalized = first
		s.recomputeFinalized()
	}
	return ids
}

// provisionalIndex returns the index of the provisional block standing for
// messageID, or -1.
func (s *Session) provisionalIndex(messageID string) int {
	if messageID == "" {
		return -1
	}
	return slices.IndexFunc(s.Blocks, func(b Block) bool {
		return b.Provisional && b.MessageID == messageID
	})
}

// recomputeFinalized advances the watermark to the first block that can still
// change. It only ever moves FORWARD: renderer.Lines treats a backwards move
// as "this is a different transcript" and throws its whole cache away.
func (s *Session) recomputeFinalized() {
	i := s.Finalized
	if i < 0 {
		i = 0
	}
	for i < len(s.Blocks) && s.Blocks[i].Settled() {
		i++
	}
	s.Finalized = i
}

// settleAll marks every block that exists now as immutable, whether or not
// its tool calls were ever answered.
//
// This is the anti-stall guard and it is not optional. A turn that ends with
// a call still outstanding — interrupted, or cut short — would otherwise park
// the watermark permanently, leaving every later block in the live region to
// be re-rendered on every tick. The live region must be bounded by the length
// of one turn; if it is ever longer, this is missing.
func (s *Session) settleAll() {
	s.Finalized = len(s.Blocks)
}

// Apply folds one event into the session, reporting whether it did.
//
// It ignores events for other children rather than trusting the caller to
// route: Phase A shipped its one Critical because every test double at every
// layer ignored the child id, and this is the layer where the check is free.
//
// The report is for state that must not move ahead of the transcript the
// session holds — the cockpit retires an optimistic send on a linked message
// and on a turn's end, and neither may happen for an event this session never
// folded in.
func (s *Session) Apply(ev *rafikiv1.Event) bool {
	if ev == nil {
		return false
	}
	if id := ev.GetChildId(); id != "" && s.ChildID != "" && id != s.ChildID {
		return false
	}
	// An ordinal we have already folded in is a duplicate, and duplicates are
	// routine rather than exceptional: the rail and focus subscriptions overlap
	// on the durable tier, so a focused child's turn_end and error events arrive
	// on BOTH. Without this an error would append its KindSystem block twice.
	// Ephemeral events carry no ordinal and are always applied.
	if ev.Ordinal != nil {
		ord := ev.GetOrdinal()
		if s.HasCursor && ord <= s.Cursor {
			return false
		}
		s.Cursor = ord
		s.HasCursor = true
	}

	s.applyPayload(ev)
	return true
}

// ApplyHistory folds in one event from GetHistory WITHOUT touching the cursor.
//
// That exclusion is the whole reason this exists rather than being Apply. The
// two ordinal spaces are unrelated: GetHistory stamps
// conversation_message.ordinal (0..N over every message ever persisted), while
// the focus stream resumes from conversations.event_log.ordinal (0..M over one
// child's logged EVENTS). A 1217-message conversation whose log holds five
// events would set the cursor to 1216, and the next subscription would resume
// past the end of the log and receive nothing, forever, with no error.
func (s *Session) ApplyHistory(ev *rafikiv1.Event) {
	if ev == nil {
		return
	}
	if id := ev.GetChildId(); id != "" && s.ChildID != "" && id != s.ChildID {
		return
	}
	s.applyPayload(ev)
}

func (s *Session) applyPayload(ev *rafikiv1.Event) {
	switch p := ev.Payload.(type) {
	case *rafikiv1.Event_UserMessage:
		s.applyUserMessage(p.UserMessage)
	case *rafikiv1.Event_AssistantMessage:
		s.applyAssistantMessage(p.AssistantMessage)
	case *rafikiv1.Event_TurnStart:
		s.Blocks = append(s.Blocks, Block{Kind: KindAssistant, At: time.Now()})
	case *rafikiv1.Event_TurnEnd:
		s.applyTurnEnd(p.TurnEnd)
		s.DropProvisional()
	case *rafikiv1.Event_ContentBlockDelta:
		s.applyDelta(p.ContentBlockDelta)
	case *rafikiv1.Event_ToolExecutionStart:
		s.applyToolStart(p.ToolExecutionStart)
	case *rafikiv1.Event_ToolExecutionEnd:
		s.applyToolEnd(p.ToolExecutionEnd)
	case *rafikiv1.Event_AgentStatus:
		s.Status = p.AgentStatus.GetState()
	case *rafikiv1.Event_Error:
		s.Blocks = append(s.Blocks, Block{
			Kind:  KindSystem,
			At:    time.Now(),
			Text:  "Error: " + p.Error.GetMessage(),
			Final: true,
		})
		s.settleAll()
	case *rafikiv1.Event_CompactionBoundary:
		// A compaction boundary is mid-conversation, not a turn-ending event:
		// unlike the Error case above, do NOT settleAll here — that would force
		// every in-flight tool call to resolve early. KindSystem (never KindUser)
		// is also what keeps the cockpit's ⏳ pending echo untouched: it clears
		// only on an appended KindUser block.
		s.Blocks = append(s.Blocks, Block{
			Kind:  KindSystem,
			At:    time.Now(),
			Text:  formatCompactionBoundary(p.CompactionBoundary),
			Final: true,
		})
	case *rafikiv1.Event_Retry:
		// The daemon's rate-limit auto-resume is the event's only producer
		// today. will_retry=true means the agent is parked waiting for a
		// scheduled retry (the rail shows ⟳): append the reason as a system
		// block so the transcript names when the resume fires. will_retry=false
		// is the resolution half (fired, cleared by a success, abandoned): it
		// only un-sticks the rail's glyph, and the turn that follows appends
		// its own blocks. Like CompactionBoundary this is mid-conversation, not
		// turn-ending — do NOT settleAll: a future mid-turn producer would
		// otherwise freeze its in-flight tool calls without their results, and
		// today's producer fires after the turn's own result already settled
		// everything settleable.
		if p.Retry.GetWillRetry() {
			s.Blocks = append(s.Blocks, Block{
				Kind:  KindSystem,
				At:    time.Now(),
				Text:  retryScheduleText(p.Retry),
				Final: true,
			})
		}
	case *rafikiv1.Event_ScriptOutput:
		// A script child's stdout/stderr. This is the script's WHOLE output
		// record -- it has no conversation rows and no turns, so unlike an LLM
		// child's events these land in the transcript as content, not as
		// commentary. stderr is visually distinct: a `stderr| ` prefix keeps a
		// run's diagnostics readable against its real output without inventing
		// a second block stream. Applied through ApplyHistory or Apply alike --
		// the payload carries no cursor semantics of its own.
		s.applyScriptOutput(p.ScriptOutput)
	case *rafikiv1.Event_ScriptReport:
		s.Blocks = append(s.Blocks, Block{
			Kind:  KindSystem,
			At:    time.Now(),
			Text:  scriptReportText(p.ScriptReport),
			Final: true,
		})
		s.recomputeFinalized()
	case *rafikiv1.Event_SystemMessage:
		s.applySystemMessage(p.SystemMessage)
	case *rafikiv1.Event_ChildExited:
		// A dead child answers no more tool calls.
		s.settleAll()
	}
}

// applyScriptOutput folds one ScriptOutput event in, JOINING it into the
// previous block when the output is mid-line: the daemon's coalescer splits
// an oversized line into pieces that carry no trailing newline, and adjacent
// events of the SAME stream whose predecessor does not end with '\n' are
// fragments of one logical line — appending the new text to the prior block
// renders the line whole instead of breaking it in two. A block whose text
// ends with '\n' opens the next block normally, so a coalesced multi-line
// event is untouched. Content-join only: no cursor or ordinal logic — the
// event's ordinal was already consumed by Apply/ApplyHistory, and a joined
// block's At stays the FIRST fragment's. Never joins across streams (a
// stderr fragment following stdout stays its own block) or into a
// non-ScriptOutput block.
func (s *Session) applyScriptOutput(so *rafikiv1.ScriptOutput) {
	if n := len(s.Blocks); n > 0 {
		last := &s.Blocks[n-1]
		if last.Kind == KindScriptOutput && last.Stream == so.GetStream() &&
			!strings.HasSuffix(last.Text, "\n") {
			last.Text += so.GetText()
			s.recomputeFinalized()
			return
		}
	}
	s.Blocks = append(s.Blocks, Block{
		Kind:   KindScriptOutput,
		At:     time.Now(),
		Text:   so.GetText(),
		Stream: so.GetStream(),
		Final:  true,
	})
	s.recomputeFinalized()
}

// applySystemMessage appends one harness-injected system-role message.
//
// It is KindInjected, never KindUser: a user-role block is what clears the
// cockpit's ⏳ pending echo, and nothing the harness says on its own behalf
// acknowledges a prompt. It is never KindSystem either -- see KindInjected.
// Like CompactionBoundary and Retry it is mid-conversation, so it does NOT
// settleAll: an injected nudge arriving between a tool_use and its
// tool_result must leave the call in flight. recomputeFinalized is safe here
// because it stops at the first block that can still change.
func (s *Session) applySystemMessage(sm *rafikiv1.SystemMessage) {
	s.Blocks = append(s.Blocks, Block{
		Kind:         KindInjected,
		At:           time.Now(),
		Text:         TextFromContent(sm.GetContent()),
		InjectedKind: firstInjectedKind(sm.GetContent()),
		Final:        true,
	})
	s.recomputeFinalized()
}

// scriptReportText renders one ScriptReport as a system line: the caller's
// own discriminator kind plus its JSON payload, folded to one line.
func scriptReportText(sr *rafikiv1.ScriptReport) string {
	text := "report: " + sr.GetKind()
	if data := sr.GetDataJson(); data != "" {
		text += " " + strings.Join(strings.Fields(data), " ")
	}
	return text
}

// retryScheduleText renders the will_retry=true schedule line. The fire
// instant travels as resume_at — the producing daemon's clock zone is
// arbitrary (a container runs UTC), so a wall-clock it embedded in reason
// would show a stranger's midnight — and is rendered here in the viewer's
// local zone (.Local()). An event with no instant (a producer that names none,
// or a row written before the field existed) falls back to the reason text
// verbatim.
func retryScheduleText(r *rafikiv1.Retry) string {
	if r.ResumeAt == nil {
		return r.GetReason()
	}
	text := fmt.Sprintf("%s; auto-resume scheduled for %s",
		r.GetReason(), r.GetResumeAt().AsTime().Local().Format("15:04:05"))
	if max := r.GetMaxAttempts(); max > 0 {
		return fmt.Sprintf("%s (attempt %d/%d)", text, r.GetAttempt(), max)
	}
	return fmt.Sprintf("%s (attempt %d)", text, r.GetAttempt())
}

// LastAssistant returns a pointer to the most recent assistant block, or nil.
func (s *Session) LastAssistant() *Block {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		if s.Blocks[i].Kind == KindAssistant {
			return &s.Blocks[i]
		}
	}
	return nil
}

// applyUserMessage appends the user's turn -- and routes any tool_result
// blocks onto the tool calls they answer.
//
// Anthropic puts tool_result in the USER message that follows the assistant's
// tool_use, so a tool-calling conversation alternates assistant/user with the
// "user" half being nothing but results. TextFromContent reads text blocks
// only, so those messages used to render as EMPTY user bubbles and the results
// were dropped on the floor -- one blank bubble per tool call, and no output
// anywhere. A message that carries only results appends no block at all.
//
// A user-role message may also carry injected blocks -- the harness's own words
// recorded alongside the user's. Those become KindInjected blocks ahead of the
// user's block, in wire order.
func (s *Session) applyUserMessage(um *rafikiv1.UserMessage) {
	// Injected blocks precede the user's own: they are the harness's words, and
	// the wire's order within each list is the order they happened. Never
	// interleave the two lists.
	var added []Block
	for _, cb := range um.GetInjected() {
		added = append(added, Block{
			Kind:         KindInjected,
			At:           time.Now(),
			Text:         TextFromContent([]*rafikiv1.ContentBlock{cb}),
			InjectedKind: cb.GetInjectedKind(),
			Final:        true,
		})
	}

	var results int
	for _, cb := range um.GetContent() {
		tr, ok := cb.Block.(*rafikiv1.ContentBlock_ToolResult)
		if !ok {
			continue
		}
		results++
		s.attachToolResult(tr.ToolResult)
	}

	text := TextFromContent(um.GetContent())
	images := imagesFrom(um.GetContent())
	// Recorded before the branches below, because a linked capture is linked
	// whether or not it leaves a block behind: one that carries no user content
	// of its own is dropped, and its id is all that stops a receipt joining
	// afterwards from inserting a block nothing will ever replace.
	s.markLinked(um.GetMessageId())
	if i := s.provisionalIndex(um.GetMessageId()); i >= 0 {
		// The daemon linked this message to the send that carried it, so its
		// provisional block already holds the position: replace it THERE. The
		// placeholder logic below exists to find that position from scratch,
		// and running it here would append a second row below the streaming
		// reply. A linked message with no user content of its own — a
		// results-only capture — removes the block and leaves only what was
		// injected alongside it.
		//
		// The watermark is reopened HERE, before either mutation, and STAYS
		// there: an error or a child exit settles every block, this one
		// included (`settleAll`), so the replace and the injected insert both
		// land below it. Left alone it would name rows that have moved, and
		// renderer.Lines only drops its cache when the watermark moves BACK
		// — recomputing it forward again in the same call would walk it past
		// the rows that just changed and leave the cached provisional row, and
		// every cached row above the insert, on screen. The next event's
		// recomputeFinalized takes it forward again, and until then the tail is
		// simply re-rendered.
		reopened := i < s.Finalized
		if reopened {
			s.Finalized = i
		}
		at := s.Blocks[i].At
		if text == "" && len(images) == 0 {
			s.Blocks = slices.Delete(s.Blocks, i, i+1)
		} else {
			s.Blocks[i] = Block{
				Kind:      KindUser,
				At:        at,
				Text:      text,
				Images:    images,
				MessageID: um.GetMessageId(),
				Final:     true,
			}
		}
		s.Blocks = slices.Insert(s.Blocks, i, added...)
		if !reopened {
			s.recomputeFinalized()
		}
		return
	}
	if text == "" && len(images) == 0 && (results > 0 || len(um.GetInjected()) > 0) {
		// A results-only message appends no KindUser block, but it just answered
		// tool calls -- which is exactly what unsticks the watermark. An
		// injected-only message likewise appends no KindUser block: a user-role
		// block is what clears the cockpit's ⏳ pending echo, and the harness
		// speaking on its own behalf acknowledges no prompt. Its injected blocks
		// still land.
		if !s.insertBeforePlaceholder(added) {
			s.recomputeFinalized()
		}
		return
	}
	added = append(added, Block{
		Kind:      KindUser,
		At:        time.Now(),
		Text:      text,
		Images:    images,
		MessageID: um.GetMessageId(),
		Final:     true,
	})
	if !s.insertBeforePlaceholder(added) {
		s.recomputeFinalized()
	}
}

// insertBeforePlaceholder adds blocks at the end of the transcript, except that
// they go AHEAD of a trailing assistant block that is open and still empty.
//
// That block is TurnStart's placeholder, opened when the prompt is delivered;
// the user message it answers is published later (a claude prompt from its
// captured row, once the response is in), and the reply then fills the
// placeholder in place. Appending behind it would leave the reply above the
// prompt that asked for it. A block that already holds streamed content is a
// reply in progress, and a steer's echo correctly lands after it.
//
// It reports whether it inserted BELOW the watermark, which happens when a
// settleAll — an error, a child exit — has already settled the placeholder this
// insert goes ahead of. The block that was there shifts down, and the renderer
// caches every block below the watermark exactly once, so the watermark is
// reopened at the insert index and the CALLER must leave it alone: recomputing
// it forward in the same call walks it back past the rows that have moved, and
// Lines then sees no backwards move and serves what it cached before the
// insert. The append branch never moves anything and never reports.
func (s *Session) insertBeforePlaceholder(blocks []Block) bool {
	if n := len(s.Blocks); n > 0 {
		if last := s.Blocks[n-1]; last.Kind == KindAssistant && !last.Final &&
			last.Text == "" && last.ThinkText == "" && len(last.ToolCalls) == 0 && len(last.Content) == 0 {
			s.Blocks = slices.Insert(s.Blocks, n-1, blocks...)
			if n-1 < s.Finalized {
				s.Finalized = n - 1
				return true
			}
			return false
		}
	}
	s.Blocks = append(s.Blocks, blocks...)
	return false
}

// attachToolResult files one result against its call, searching backwards:
// a turn's results arrive in the message AFTER the assistant block that holds
// the calls, and with parallel tool use several may answer calls made in the
// same block.
func (s *Session) attachToolResult(tr *rafikiv1.ToolResultBlock) {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		if s.Blocks[i].Kind != KindAssistant {
			continue
		}
		for j := range s.Blocks[i].ToolCalls {
			if s.Blocks[i].ToolCalls[j].ID != tr.GetToolUseId() {
				continue
			}
			s.Blocks[i].ToolCalls[j].Result = TextFromContent(tr.GetContent())
			s.Blocks[i].ToolCalls[j].HasResult = true
			s.Blocks[i].ToolCalls[j].Images = imagesFrom(tr.GetContent())
			// Never DOWNGRADE a failure. tool_execution_end may already have
			// said this call failed, and it is the more direct witness — it
			// carries the tool's own error. A stored tool_result block whose
			// is_error is absent or false would otherwise turn a ✗ back into a
			// ✓, which is the one direction that must never happen silently.
			if tr.GetIsError() {
				s.Blocks[i].ToolCalls[j].IsError = true
			}
			s.Blocks[i].ToolCalls[j].Running = false
			return
		}
	}
}

func (s *Session) applyAssistantMessage(am *rafikiv1.AssistantMessage) {
	block := Block{
		Kind:       KindAssistant,
		At:         time.Now(),
		Content:    am.GetContent(),
		StopReason: am.GetRawStopReason(),
		Final:      true,
	}
	for _, cb := range am.GetContent() {
		switch b := cb.Block.(type) {
		case *rafikiv1.ContentBlock_Text:
			block.Text += b.Text.GetText()
		case *rafikiv1.ContentBlock_Thinking:
			block.ThinkText += b.Thinking.GetThinking()
		case *rafikiv1.ContentBlock_ToolUse:
			block.ToolCalls = append(block.ToolCalls, ToolCall{
				ID:    b.ToolUse.GetId(),
				Name:  b.ToolUse.GetName(),
				Input: b.ToolUse.GetInputJson(),
			})
		case *rafikiv1.ContentBlock_Image:
			block.Images = append(block.Images, b.Image)
		case *rafikiv1.ContentBlock_ToolResult:
			for i := range block.ToolCalls {
				if block.ToolCalls[i].ID == b.ToolResult.GetToolUseId() {
					block.ToolCalls[i].IsError = b.ToolResult.GetIsError()
					block.ToolCalls[i].Result = TextFromContent(b.ToolResult.GetContent())
					block.ToolCalls[i].HasResult = true
					block.ToolCalls[i].Images = imagesFrom(b.ToolResult.GetContent())
				}
			}
		}
	}
	// A streamed block, or the placeholder TurnStart opened, is waiting for
	// exactly this message: REPLACE it rather than appending a second one.
	// Appending renders the reply twice — once as the text the deltas already
	// accumulated, once as the committed message — and leaves an empty block
	// ahead of the real one for a turn that never streamed at all (a claude
	// child, whose message arrives from its captured row with no deltas
	// before it).
	//
	// The replacement is WHOLE: Content, Text, ThinkText, ToolCalls, Images,
	// StopReason and Final all come from the message, and everything the deltas
	// accumulated is discarded. That is the point — the deltas only ever
	// carried text and thinking, in fragments, with no argument objects, no
	// images and no stop reason, so keeping any of it would mean trusting a
	// partial copy over the authoritative one.
	//
	// It is the last ASSISTANT block that is replaced, not necessarily the last
	// block: a steer echoed mid-stream appends a user block while the deltas go
	// on filling the assistant one, and the committed message still belongs to
	// that block (see openAssistantIndex).
	if i := s.openAssistantIndex(); i >= 0 {
		s.Blocks[i] = block
		s.recomputeFinalized()
		return
	}
	s.Blocks = append(s.Blocks, block)
	s.recomputeFinalized()
}

// openAssistantIndex finds the most recent assistant block that is still open —
// the one deltas are being appended to — or -1 when the last assistant block is
// already final (or there is none).
//
// It walks past a NON-assistant block on purpose. A steer's echo is a
// user_message published mid-turn, so it lands in the transcript while the
// assistant message it interrupted is still streaming; requiring the open block
// to be the very last one would append the committed message as a second
// assistant block instead of completing the first.
//
// Final, not "the last assistant block", is the gate: a tool round commits its
// message before the next one starts streaming, so the previous call's block is
// final and a new call must NOT overwrite it.
func (s *Session) openAssistantIndex() int {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		if s.Blocks[i].Kind != KindAssistant {
			continue
		}
		if s.Blocks[i].Final {
			return -1
		}
		return i
	}
	return -1
}

func (s *Session) applyTurnEnd(te *rafikiv1.TurnEnd) {
	if last := s.LastAssistant(); last != nil {
		last.Final = true
		if te.StopReason != rafikiv1.StopReason_STOP_REASON_UNSPECIFIED {
			last.StopReason = te.GetRawStopReason()
		}
	}
	s.settleAll()
}

// applyDelta appends one streamed fragment to the assistant block being built.
//
// It OPENS that block when the most recent one is already final. A tool-calling
// turn makes one LLM call per round, and each call's message commits (and
// finalizes) before the next call starts streaming, so appending to the last
// assistant block would splice the second call's text into the first call's.
// TurnStart's own placeholder is not final, so the first call appends to it.
func (s *Session) applyDelta(delta *rafikiv1.ContentBlockDelta) {
	last := s.LastAssistant()
	if last == nil || last.Final {
		s.Blocks = append(s.Blocks, Block{Kind: KindAssistant, At: time.Now()})
		last = &s.Blocks[len(s.Blocks)-1]
	}
	switch d := delta.Delta.(type) {
	case *rafikiv1.ContentBlockDelta_Text:
		last.Text += d.Text
	case *rafikiv1.ContentBlockDelta_Thinking:
		last.ThinkText += d.Thinking
	case *rafikiv1.ContentBlockDelta_InputJson:
		if len(last.ToolCalls) > 0 {
			last.ToolCalls[len(last.ToolCalls)-1].Input += d.InputJson
		}
	}
}

// applyToolStart marks a call running, appending it only if it is not already
// known.
//
// It is normally NOT new: the assistant message carrying the tool_use block is
// published before the tool runs, so the call is already in the block and this
// event only says execution began. Appending unconditionally listed every tool
// call twice — invisible for as long as fundi published no assistant messages
// at all, and immediate once it did.
func (s *Session) applyToolStart(ts *rafikiv1.ToolExecutionStart) {
	last := s.LastAssistant()
	if last == nil {
		return
	}
	for i := range last.ToolCalls {
		if last.ToolCalls[i].ID == ts.GetToolUseId() {
			last.ToolCalls[i].Running = true
			return
		}
	}
	last.ToolCalls = append(last.ToolCalls, ToolCall{
		ID:      ts.GetToolUseId(),
		Name:    ts.GetName(),
		Running: true,
	})
}

func (s *Session) applyToolEnd(te *rafikiv1.ToolExecutionEnd) {
	last := s.LastAssistant()
	if last == nil {
		return
	}
	for i := range last.ToolCalls {
		if last.ToolCalls[i].ID == te.GetToolUseId() {
			last.ToolCalls[i].Running = false
			last.ToolCalls[i].DurationMs = te.GetDuration().AsDuration().Milliseconds()
			last.ToolCalls[i].IsError = te.GetIsError()
		}
	}
}

// TextFromContent concatenates the text of every text block in content.
// Images are carried separately (Block.Images, ToolCall.Images) and named by
// the renderer, which never draws an image as nothing.
func TextFromContent(content []*rafikiv1.ContentBlock) string {
	var sb strings.Builder
	for _, cb := range content {
		if b, ok := cb.Block.(*rafikiv1.ContentBlock_Text); ok {
			sb.WriteString(b.Text.GetText())
		}
	}
	return sb.String()
}

// firstInjectedKind returns the first kind named by content's blocks, or "".
// A system-role message carries its blocks in one list rather than a separate
// injected one, so the kind is a property of the block, not of the message.
func firstInjectedKind(content []*rafikiv1.ContentBlock) string {
	for _, cb := range content {
		if k := cb.GetInjectedKind(); k != "" {
			return k
		}
	}
	return ""
}

// imagesFrom returns content's top-level image blocks, in order. It does not
// descend into tool_result blocks: those images belong to the call.
func imagesFrom(content []*rafikiv1.ContentBlock) []*rafikiv1.ImageBlock {
	var out []*rafikiv1.ImageBlock
	for _, cb := range content {
		if img := cb.GetImage(); img != nil {
			out = append(out, img)
		}
	}
	return out
}

// writeImages adds an image's identity to a fingerprint: media type plus data
// length, not a hash — Fingerprint runs on every live block every frame.
func writeImages(sb *strings.Builder, imgs []*rafikiv1.ImageBlock) {
	for _, img := range imgs {
		sb.WriteString("img")
		sb.WriteString(img.GetMediaType())
		sb.WriteString(strconv.Itoa(len(img.GetData())))
	}
}

// ImagePlaceholder names an image the transcript cannot draw.
//
// bubbletea v2 renders through ultraviolet's cell grid — one grapheme, a
// style, a hyperlink and a width per cell — so a graphics escape sequence
// written into a View is parsed into cells, has nowhere to live, and is
// dropped: measured over a pty, the text either side of an iTerm2 inline-image
// sequence survives and the sequence itself does not.
//
// That is not the last word on images, only on escape sequences. Kitty's
// UNICODE PLACEHOLDER protocol transmits the image out of band and then places
// it with ordinary printable runes (U+10EEEE plus diacritics encoding the row,
// column and image id), which pass through a cell renderer untouched because
// they are just text. The renderer draws those rows where the terminal
// supports them (pkg/tui/kittyimg.go) and this line everywhere else — in
// placeholder mode, before a thumbnail is ready, when one failed, and when a
// pane is too narrow.
func ImagePlaceholder(img *rafikiv1.ImageBlock) string {
	if img == nil {
		return ""
	}
	media := img.GetMediaType()
	if media == "" {
		media = "image"
	}
	out := "🖼 " + media
	// DecodeConfig reads only the header, so this costs a few hundred bytes of
	// parsing rather than decoding the pixels.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(img.GetData())); err == nil {
		out += " " + itoa(cfg.Width) + "×" + itoa(cfg.Height)
	}
	if n := len(img.GetData()); n > 0 {
		out += " (" + humanBytes(n) + ")"
	}
	return out
}

// formatCompactionBoundary renders the two token-format variants design §7
// specifies: live events carry both pre and post tokens; a reattach-
// synthesized event (from a stored kind='compaction_summary' row) carries
// only pre, approximate. N/M are tokens/1000, integer division.
func formatCompactionBoundary(cb *rafikiv1.CompactionBoundary) string {
	pre, post := cb.PreTokens, cb.PostTokens
	switch {
	case cb.Trigger == "clear":
		return "— context cleared —"
	case pre != nil && post != nil:
		return fmt.Sprintf("— context compacted · %dk → %dk tokens —", *pre/1000, *post/1000)
	case pre != nil:
		return fmt.Sprintf("— context compacted · ~%dk tokens —", *pre/1000)
	default:
		return "— context compacted —"
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.Itoa(n) + " B"
	}
}
