package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gp/internal/ai"
)

func TestCosyEncodeDecodeRoundTrip(t *testing.T) {
	orig := []byte(`{"model":"auto","messages":[{"role":"user","content":"hello world"}]}`)
	enc := EncodeBody(orig)
	dec, err := DecodeBody(enc)
	if err != nil {
		t.Fatalf("DecodeBody failed: %v", err)
	}
	if string(dec) != string(orig) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", string(dec), string(orig))
	}
}

func TestQoderProvider_StreamChat_MockUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathJobExchange:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"dt-mock-token","refresh_token":"jrt-mock","expires_in":3600000}`))
		case pathUserInfo:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"uid-123","name":"tester","user_type":"personal_professional_trial"}`))
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
				t.Fatalf("expected COSY bearer header, got %q", r.Header.Get("Authorization"))
			}
			rawBody, _ := io.ReadAll(r.Body)
			dec, err := DecodeBody(string(rawBody))
			if err != nil {
				t.Fatalf("DecodeBody failed: %v", err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(dec, &parsed); err != nil {
				t.Fatalf("decoded JSON invalid: %v", err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			chunk1 := `{"choices":[{"delta":{"reasoning_content":"let me think"}}]}`
			chunk2 := `{"choices":[{"delta":{"content":"hello from qoder"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
			env1, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": chunk1})
			env2, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": chunk2})
			envDone, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": "[DONE]"})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: %s\n\n", env1, env2, envDone)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	prov := NewProvider("pt-mock-pat")
	prov.SetHTTPClient(srv.Client())
	prov.SetRealms([]realmEndpoint{{
		Name:    "test",
		OpenAPI: srv.URL,
		Gateway: srv.URL,
		UA:      "Qoder/1.1.75",
	}})

	ch, err := prov.StreamChat(context.Background(), &ai.ChatRequest{
		Model:         "auto",
		ThinkingLevel: "medium",
		Messages:      []ai.Message{ai.NewUserMessage("hi")},
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	var gotText, gotThink string
	var gotTotal int64
	for c := range ch {
		if c.Error != nil {
			t.Fatalf("unexpected chunk error: %v", c.Error)
		}
		gotThink += c.ThinkingDelta
		gotText += c.DeltaText
		if c.Usage != nil {
			gotTotal = c.Usage.TotalTokens
		}
	}
	if gotThink != "let me think" || gotText != "hello from qoder" || gotTotal != 15 {
		t.Fatalf("unexpected stream output: think=%q text=%q total=%d", gotThink, gotText, gotTotal)
	}
}
