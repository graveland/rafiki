package tools

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// questionOperatorSentence is the exact sentence the plan appends to
// agent_report's description: with no parent the note reaches the operator,
// and there a question means the child is stopping until answered.
const questionOperatorSentence = ` With no parent, your note reaches the operator instead; there, kind "question" means you are stopping until they answer, so send it and end your turn.`

// TestAgentReportDescriptionMentionsOperator pins the fundi face's wording: the
// description the child's model reads carries the exact operator sentence, so a
// top-level child knows a question reaches the operator and stops its turn.
func TestAgentReportDescriptionMentionsOperator(t *testing.T) {
	ck := assert.NewAborting(t)
	desc := AgentReportBlueprint{}.Description()
	ck.True(strings.Contains(desc, questionOperatorSentence),
		"the fundi agent_report description must carry the operator sentence; got %q", desc)
}
