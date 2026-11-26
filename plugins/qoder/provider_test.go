package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestResolveExpiry_UnitAdaptive(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		expiresIn   int64
		expiresInMs int64
		want        time.Duration
	}{
		{"seconds 3600", 3600, 0, time.Hour},
		{"milliseconds 3600000", 3600000, 0, time.Hour},
		{"explicit ms field wins", 3600, 7200000, 2 * time.Hour},
		{"seconds 900", 900, 0, 15 * time.Minute},
		{"tiny ms value clamped to min", 0, 500, minTokenTTL},
		{"absent falls back to default", 0, 0, defaultTokenTTL},
		{"huge value clamped to max", 9_999_999_999, 0, maxTokenTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveExpiry(now, tc.expiresIn, tc.expiresInMs).Sub(now)
			if got != tc.want {
				t.Fatalf("resolveExpiry(expires_in=%d, expires_in_ms=%d) = %v, want %v",
					tc.expiresIn, tc.expiresInMs, got, tc.want)
			}
		})
	}
}

func TestExchangePAT_AggregatesAllRealmErrors(t *testing.T) {
	newFailing := func(realm string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"errorCode":"BadRequest","errorMessage":"%s says no","requestId":"rid-%s"}`, realm, realm)
		}))
	}
	intl, cn := newFailing("intl"), newFailing("cn")
	defer intl.Close()
	defer cn.Close()

	prov := NewProvider("pt-mock-pat")
	prov.SetHTTPClient(intl.Client())
	prov.SetRealms([]realmEndpoint{
		{Name: "intl", OpenAPI: intl.URL, Gateway: intl.URL, UA: "Qoder/1.1.75"},
		{Name: "cn", OpenAPI: cn.URL, Gateway: cn.URL, UA: "QoderWork/1.1.75"},
	})

	_, err := prov.StreamChat(context.Background(), &ai.ChatRequest{
		Model:    "auto",
		Messages: []ai.Message{ai.NewUserMessage("hi")},
	})
	if err == nil {
		t.Fatal("expected exchange failure, got nil")
	}
	// 两个 realm 的原因都必须保留：只留最后一个会让排查时看不到 intl 的真实错误。
	for _, want := range []string{"realm intl", "realm cn", "intl says no", "cn says no"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("aggregated error missing %q: %v", want, err)
		}
	}
}

func TestExchangePAT_NonPATCredentialGetsHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	prov := NewProvider("sk-not-a-pat")
	prov.SetHTTPClient(srv.Client())
	prov.SetRealms([]realmEndpoint{{Name: "cn", OpenAPI: srv.URL, Gateway: srv.URL, UA: "QoderWork/1.1.75"}})

	_, err := prov.StreamChat(context.Background(), &ai.ChatRequest{
		Model:    "auto",
		Messages: []ai.Message{ai.NewUserMessage("hi")},
	})
	if err == nil || !strings.Contains(err.Error(), "pt- 开头") {
		t.Fatalf("expected pt- PAT hint, got: %v", err)
	}
}

func TestProvider_AuthFailureBackoff(t *testing.T) {
	var exchanges int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathJobExchange {
			atomic.AddInt32(&exchanges, 1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	prov := NewProvider("pt-mock-pat")
	prov.SetHTTPClient(srv.Client())
	prov.SetRealms([]realmEndpoint{{Name: "cn", OpenAPI: srv.URL, Gateway: srv.URL, UA: "QoderWork/1.1.75"}})

	req := &ai.ChatRequest{Model: "auto", Messages: []ai.Message{ai.NewUserMessage("hi")}}
	for range 3 {
		if _, err := prov.StreamChat(context.Background(), req); err == nil {
			t.Fatal("expected failure")
		}
	}
	if got := atomic.LoadInt32(&exchanges); got != 1 {
		t.Fatalf("expected 1 exchange attempt within backoff window, got %d", got)
	}

	// 显式强制刷新必须绕过冷却，否则用户换完凭证还要等 30 秒。
	if _, err := prov.getAuth(context.Background(), true); err == nil {
		t.Fatal("expected forced refresh to fail")
	}
	if got := atomic.LoadInt32(&exchanges); got != 2 {
		t.Fatalf("expected forced refresh to hit upstream, got %d exchanges", got)
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

func TestQoderProvider_MapsCacheUsageFields(t *testing.T) {
	p := NewProvider("pt-mock-pat")
	cases := []struct {
		name           string
		usageJSON      string
		wantIn         int64
		wantCacheRead  int64
		wantCacheWrite int64
	}{
		{
			name:          "prompt_tokens_details_cached_tokens",
			usageJSON:     `{"prompt_tokens":180000,"completion_tokens":50,"total_tokens":180050,"prompt_tokens_details":{"cached_tokens":150000,"cache_creation_tokens":1200}}`,
			wantIn:        180000,
			wantCacheRead: 150000,
			wantCacheWrite: 1200,
		},
		{
			name:          "anthropic_or_gateway_cache_fields",
			usageJSON:     `{"prompt_tokens":12000,"completion_tokens":40,"total_tokens":12040,"cache_read_input_tokens":9500,"cache_creation_input_tokens":2000}`,
			wantIn:        12000,
			wantCacheRead: 9500,
			wantCacheWrite: 2000,
		},
		{
			name:          "deepseek_prompt_cache_hit_tokens",
			usageJSON:     `{"prompt_tokens":5000,"completion_tokens":20,"total_tokens":5020,"prompt_cache_hit_tokens":4096}`,
			wantIn:        5000,
			wantCacheRead: 4096,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":` + tc.usageJSON + `}`
			outerBytes, _ := json.Marshal(map[string]any{"body": inner, "statusCodeValue": 200})
			streamBody := "data: " + string(outerBytes) + "\ndata: [DONE]\n"

			ch := make(chan ai.StreamChunk, 8)
			p.readSSEStream(context.Background(), io.NopCloser(strings.NewReader(streamBody)), ch)

			var gotUsage *ai.TokenUsage
			for c := range ch {
				if c.Error != nil {
					t.Fatalf("unexpected error: %v", c.Error)
				}
				if c.Usage != nil {
					gotUsage = c.Usage
				}
			}
			if gotUsage == nil {
				t.Fatal("expected non-nil Usage chunk")
			}
			if gotUsage.InputTokens != tc.wantIn || gotUsage.CacheReadTokens != tc.wantCacheRead || gotUsage.CacheWriteTokens != tc.wantCacheWrite {
				t.Fatalf("got usage %+v, want in=%d read=%d write=%d", *gotUsage, tc.wantIn, tc.wantCacheRead, tc.wantCacheWrite)
			}
		})
	}
}
