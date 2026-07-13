package store

// DrivenBy is who drives a conversation's history: the library (stateful,
// message rows + turns) or a pass-through proxy client that owns its own
// history (turns only). Stamped at conversation creation, immutable.
type DrivenBy string

const (
	DrivenByServer DrivenBy = "server"
	DrivenByClient DrivenBy = "client"
)

// TurnStatus is the write-ahead lifecycle of a conversation_turn row.
type TurnStatus string

const (
	TurnPending  TurnStatus = "pending"
	TurnComplete TurnStatus = "complete"
	TurnError    TurnStatus = "error"
)

// Protocol is the wire protocol of a captured turn.
type Protocol string

const (
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolOpenAI    Protocol = "openai"
)

// ConversationStatus is the conversation lifecycle state.
type ConversationStatus string

const (
	ConversationActive ConversationStatus = "active"
	ConversationFailed ConversationStatus = "failed"
)
