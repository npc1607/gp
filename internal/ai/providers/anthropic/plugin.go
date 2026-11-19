package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gp/internal/ai"
)

func init() {
	ai.RegisterProviderPlugin(ai.ProviderPlugin{
		ID:          "anthropic",
		Aliases:     []string{"claude"},
		DisplayName: "Anthropic",
		DefaultURL:  defaultBaseURL,
		KeyHint:     "sk-ant-...",
		EnvKeys:     []string{"ANTHROPIC_API_KEY", "CLAUDE_API_KEY"},
		DefaultModels: []string{
			"claude-sonnet-4-5",
			"claude-opus-4-1",
			"claude-haiku-4-5",
			"claude-3-7-sonnet-latest",
		},
		InferModel: func(model string) bool {
			return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "claude")
		},
		MatchCredential: func(apiKey, baseURL, model string) bool {
			if strings.HasPrefix(strings.TrimSpace(apiKey), "sk-ant-") {
				return true
			}
			if strings.Contains(strings.ToLower(baseURL), "anthropic") {
				return true
			}
			// 仅当既无自定义端点、又显式指定 claude 模型时才接管，避免抢占其他插件的空 baseURL 场景
			return strings.TrimSpace(baseURL) == "" && strings.Contains(strings.ToLower(model), "claude")
		},
		NewProvider: func(apiKey, baseURL string) ai.Provider {
			return NewProvider(apiKey, baseURL)
		},
		FetchModels: fetchModels,
	})
}

// fetchModels 通过 GET /v1/models 拉取线上模型目录
func fetchModels(apiKey, baseURL string) ([]string, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		endpoint = defaultBaseURL
	}
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/v1"
	}
	endpoint += "/models?limit=100"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build models request: %w", err)
	}
	req.Header.Set("anthropic-version", apiVersion)
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: fetch models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic: fetch models status %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("anthropic: decode models: %w", err)
	}

	ids := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("anthropic: no models returned")
	}
	return ids, nil
}
