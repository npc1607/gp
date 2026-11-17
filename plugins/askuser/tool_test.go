package askuser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gp/internal/tools"
	"gp/internal/tui"
)

func TestAskUserQuestion_ValidationAndEnvelope(t *testing.T) {
	tool := &AskUserQuestionTool{}
	if tool.Name() != "ask_user_question" {
		t.Fatalf("unexpected tool name %q", tool.Name())
	}

	// Reserved label should fail validation
	badArgs := json.RawMessage(`{"questions":[{"question":"Pick?","header":"Pick","options":[{"label":"Other","description":"bad"},{"label":"A","description":"ok"}]}]}`)
	if _, err := tool.Execute(&tools.ToolContext{Context: context.Background()}, badArgs); err == nil {
		t.Fatalf("expected validation error for reserved label 'Other'")
	}

	// Valid interactive round-trip via UIEventSender
	tools.SetUIEventSender(func(msg any) {
		qMsg, ok := msg.(tui.AskUserQuestionMsg)
		if !ok {
			return
		}
		qMsg.ResponseCh <- tui.AskUserResult{
			Answers: []tui.AskUserAnswer{
				{
					QuestionIndex: 0,
					Question:      qMsg.Questions[0].Question,
					Kind:          "option",
					Answer:        "JWT",
					Notes:         "rotate weekly",
				},
			},
			GlobalNote: "keep it simple",
		}
	})
	defer tools.SetUIEventSender(nil)

	goodArgs := json.RawMessage(`{"questions":[{"question":"Auth method?","header":"Auth","options":[{"label":"JWT","description":"stateless"},{"label":"Session","description":"cookie"}]}]}`)
	res, err := tool.Execute(&tools.ToolContext{Context: context.Background()}, goodArgs)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	for _, want := range []string{
		EnvelopePrefix,
		`"Auth method?"="JWT"`,
		"user notes: rotate weekly",
		"global note: keep it simple",
		EnvelopeSuffix,
	} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("response %q missing %q", res.Content, want)
		}
	}
}
