package askuser

import (
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/tools"
	"gp/internal/tui"
)

const (
	MaxQuestions    = 4
	MinOptions      = 2
	MaxOptions      = 4
	MaxHeaderLength = 16
	MaxLabelLength  = 60

	DeclineMessage = "User declined to answer questions"
	EnvelopePrefix = "User has answered your questions:"
	EnvelopeSuffix = "You can now continue with the user's answers in mind."
)

var reservedLabels = map[string]bool{
	"other":           true,
	"type something.": true,
	"type something":  true,
	"next":            true,
}

func init() {
	tools.RegisterPluginTool(func() tools.Tool {
		return &AskUserQuestionTool{}
	})
}

// AskUserQuestionTool 对齐 @juicesharp/rpiv-ask-user-question 的结构化问答插件工具
type AskUserQuestionTool struct{}

var _ tools.Tool = (*AskUserQuestionTool)(nil)

func (t *AskUserQuestionTool) Name() string {
	return "ask_user_question"
}

func (t *AskUserQuestionTool) Description() string {
	return "Ask the user 1-4 structured questions with 2-4 options each (plus automatic 'Type something.' free-text row) instead of guessing ambiguous requirements."
}

func (t *AskUserQuestionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"questions": {
				"type": "array",
				"minItems": 1,
				"maxItems": 4,
				"description": "Questions to ask the user (1-4 questions)",
				"items": {
					"type": "object",
					"properties": {
						"question": {
							"type": "string",
							"description": "The complete question to ask the user."
						},
						"header": {
							"type": "string",
							"maxLength": 16,
							"description": "Short chip/tag (max 16 chars) shown in tab bar, e.g. 'Auth method'."
						},
						"options": {
							"type": "array",
							"minItems": 2,
							"maxItems": 4,
							"description": "2-4 choices. 'Type something.' is appended automatically.",
							"items": {
								"type": "object",
								"properties": {
									"label": { "type": "string", "maxLength": 60 },
									"description": { "type": "string" },
									"preview": { "type": "string" }
								},
								"required": ["label", "description"]
							}
						},
						"multiSelect": {
							"type": "boolean",
							"default": false
						}
					},
					"required": ["question", "header", "options"]
				}
			}
		},
		"required": ["questions"]
	}`)
}

func (t *AskUserQuestionTool) ReplayMode() tools.ReplayMode {
	return tools.ReplayInterrupt
}

type paramsInput struct {
	Questions []tui.AskUserQuestion `json:"questions"`
}

func validateQuestions(qs []tui.AskUserQuestion) error {
	if len(qs) < 1 || len(qs) > MaxQuestions {
		return fmt.Errorf("questions must contain between 1 and %d items, got %d", MaxQuestions, len(qs))
	}
	for i, q := range qs {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("question[%d].question cannot be empty", i)
		}
		if len([]rune(q.Header)) > MaxHeaderLength {
			return fmt.Errorf("question[%d].header exceeds %d characters", i, MaxHeaderLength)
		}
		if len(q.Options) < MinOptions || len(q.Options) > MaxOptions {
			return fmt.Errorf("question[%d].options must have %d-%d options, got %d", i, MinOptions, MaxOptions, len(q.Options))
		}
		for j, opt := range q.Options {
			lbl := strings.TrimSpace(opt.Label)
			if lbl == "" {
				return fmt.Errorf("question[%d].options[%d].label cannot be empty", i, j)
			}
			if len([]rune(lbl)) > MaxLabelLength {
				return fmt.Errorf("question[%d].options[%d].label exceeds %d characters", i, j, MaxLabelLength)
			}
			if reservedLabels[strings.ToLower(lbl)] {
				return fmt.Errorf("question[%d].options[%d].label %q is reserved", i, j, opt.Label)
			}
		}
	}
	return nil
}

func (t *AskUserQuestionTool) Execute(ctx *tools.ToolContext, args json.RawMessage) (*tools.ToolResult, error) {
	var in paramsInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid ask_user_question arguments: %w", err)
	}
	if err := validateQuestions(in.Questions); err != nil {
		return nil, err
	}

	respCh := make(chan tui.AskUserResult, 1)
	if !tools.SendUIEvent(tui.AskUserQuestionMsg{
		Questions:  in.Questions,
		ResponseCh: respCh,
	}) {
		return &tools.ToolResult{
			Content: DeclineMessage + " (non-interactive mode)",
			IsError: false,
		}, nil
	}

	select {
	case res := <-respCh:
		return &tools.ToolResult{
			Content: BuildQuestionnaireResponse(res, in.Questions),
		}, nil
	case <-ctx.Context.Done():
		return &tools.ToolResult{
			Content: DeclineMessage,
		}, ctx.Context.Err()
	}
}

// BuildQuestionnaireResponse 按照 rpiv-ask-user-question 规范构造返回给模型的结构化文本信封
func BuildQuestionnaireResponse(res tui.AskUserResult, questions []tui.AskUserQuestion) string {
	if res.Cancelled {
		return DeclineMessage
	}
	byIdx := make(map[int]tui.AskUserAnswer, len(res.Answers))
	for _, a := range res.Answers {
		byIdx[a.QuestionIndex] = a
	}

	var segments []string
	var unanswered []string
	for i, q := range questions {
		a, ok := byIdx[i]
		if !ok {
			unanswered = append(unanswered, fmt.Sprintf("%q", q.Question))
			continue
		}
		var scalar string
		if a.Kind == "multi" {
			parts := append([]string(nil), a.Selected...)
			if strings.TrimSpace(a.Answer) != "" {
				parts = append(parts, strings.TrimSpace(a.Answer))
			}
			scalar = strings.Join(parts, ", ")
		} else {
			scalar = a.Answer
		}
		segParts := []string{fmt.Sprintf("%q=%q", a.Question, scalar)}
		if a.Preview != "" {
			segParts = append(segParts, "selected preview: "+a.Preview)
		}
		if a.Notes != "" {
			segParts = append(segParts, "user notes: "+a.Notes)
		}
		segments = append(segments, strings.Join(segParts, ". ")+".")
	}

	hasGlobal := strings.TrimSpace(res.GlobalNote) != ""
	if len(segments) == 0 && !hasGlobal {
		return DeclineMessage
	}
	if len(unanswered) > 0 {
		segments = append(segments, "unanswered: "+strings.Join(unanswered, ", ")+".")
	}
	if hasGlobal {
		segments = append(segments, "global note: "+res.GlobalNote+".")
	}
	return EnvelopePrefix + " " + strings.Join(segments, " ") + " " + EnvelopeSuffix
}
