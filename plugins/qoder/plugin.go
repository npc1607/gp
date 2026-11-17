package qoder

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
		ID:          "qoder",
		Aliases:     []string{"qcorder"},
		DisplayName: "Qoder",
		DefaultURL:  "https://api.qoder.com/v1",
		KeyHint:     "pt-... or Qoder API Key",
		EnvKeys:     []string{"QODER_PERSONAL_ACCESS_TOKEN", "QODER_PAT", "QODER_API_KEY", "QCORDER_API_KEY"},
		DefaultModels: []string{
			"auto",
			"ultimate",
			"fast",
			"efficient",
			"smodel",
			"cmodel",
			"qmodel_latest",
			"kmodel_latest",
			"gmodel",
			"dmodel",
			"dfmodel",
			"mmodel",
		},
		InferModel: func(model string) bool {
			m := strings.ToLower(strings.TrimSpace(model))
			switch m {
			case "auto", "ultimate", "fast", "efficient", "performance",
				"smodel", "cmodel", "qmodel_latest", "qmodel_38max", "qfmodel",
				"kmodel_latest", "kmodel", "gmodel", "gfmodel", "dmodel", "dfmodel", "mmodel":
				return true
			}
			return strings.Contains(m, "qoder") || strings.Contains(m, "qcorder")
		},
		MatchCredential: func(apiKey, baseURL, model string) bool {
			if strings.HasPrefix(strings.TrimSpace(apiKey), "pt-") {
				return true
			}
			return baseURL == "" || baseURL == "https://api.qoder.com/v1"
		},
		NewProvider: func(apiKey, baseURL string) ai.Provider {
			return NewProvider(apiKey)
		},
		FetchModels: fetchQoderModels,
	})
}

func fetchQoderModels(apiKey, baseURL string) ([]string, error) {
	endpoints := []string{
		"https://api.qoder.com/api/v1/forward/models",
		"https://api.qoder.com/api/v1/cloud/models",
	}
	if strings.TrimSpace(baseURL) != "" {
		endpoints = append(endpoints, strings.TrimRight(baseURL, "/")+"/models")
	}

	client := &http.Client{Timeout: 4 * time.Second}
	for _, ep := range endpoints {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		var body struct {
			Data []struct {
				ID        string `json:"id"`
				IsEnabled *bool  `json:"is_enabled,omitempty"`
			} `json:"data"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusOK || decErr != nil || len(body.Data) == 0 {
			continue
		}
		var ids []string
		for _, item := range body.Data {
			if item.ID == "" {
				continue
			}
			if item.IsEnabled != nil && !*item.IsEnabled {
				continue
			}
			ids = append(ids, item.ID)
		}
		if len(ids) > 0 {
			return ids, nil
		}
	}
	return nil, fmt.Errorf("qoder: no models returned")
}
