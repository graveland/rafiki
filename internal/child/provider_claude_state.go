package child

// claudeProvider is the per-child stateful translator returned by
// ClaudeProvider.Fresh(). It embeds the stateless ClaudeProvider so it inherits
// BootstrapFrame / Parse / EncodeOutbound / Fresh unchanged, and overrides
// BusFrames to translate claude's raw stream-json into the pi AgentSessionEvent
// sequence (filled in Task 3). State fields accumulate across the child's
// lifetime; access is single-goroutine (readStdout), so no locking is needed.
type claudeProvider struct {
	ClaudeProvider

	// claudeState carries the translation accumulators (added in Task 3).
	st claudeState
}

// claudeState holds the per-child translation accumulators. Populated in Task 3.
type claudeState struct{}

// newClaudeProvider constructs a fresh per-child claude translator.
func newClaudeProvider() *claudeProvider { return &claudeProvider{} }

// BusFrames translates one raw claude stdout line into pi AgentSessionEvent
// frames. Stubbed to nil in Task 2; the real translation contract is
// implemented in Task 3.
func (p *claudeProvider) BusFrames(_ []byte, _ int64) [][]byte { return nil }
