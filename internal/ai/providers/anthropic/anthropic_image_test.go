package anthropic

import (
	"encoding/json"
	"testing"

	"gp/internal/ai"
)

func TestBuildMessagesSerializesToolImages(t *testing.T) {
	t.Parallel()

	p := &Provider{}
	req := &ai.ChatRequest{
		Messages: []ai.Message{
			{
				Role:       ai.RoleTool,
				ToolCallID: "call_1",
				Content:    "[Image loaded: sample.png (image/png, 68 bytes)]",
				Images: []ai.ImagePart{
					{
						MimeType:   "image/png",
						Base64Data: "AQID",
						FileName:   "sample.png",
					},
				},
			},
		},
	}

	_, msgs := p.buildMessages(req)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected role user for tool_result, got %q", msgs[0].Role)
	}

	raw, err := json.Marshal(msgs[0].Content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   []struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Source *struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "call_1" {
		t.Fatalf("unexpected tool_result block: %s", string(raw))
	}
	if len(blocks[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks (text + image), got %d", len(blocks[0].Content))
	}
	imgBlock := blocks[0].Content[1]
	if imgBlock.Type != "image" || imgBlock.Source == nil {
		t.Fatalf("expected image block with source, got %+v", imgBlock)
	}
	if imgBlock.Source.Type != "base64" || imgBlock.Source.MediaType != "image/png" || imgBlock.Source.Data != "AQID" {
		t.Fatalf("unexpected image source: %+v", imgBlock.Source)
	}
}
