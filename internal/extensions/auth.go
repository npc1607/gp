package extensions

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"gp/internal/core/auth"
)

// AuthResolver 统一解析 env://、auth://、cmd:// 三类凭证引用
type AuthResolver struct {
	AuthStore *auth.Store
}

// Resolve 解析 AuthSpec 得到内存态凭证，绝不修改原始配置
func (r *AuthResolver) Resolve(ctx context.Context, spec AuthSpec) (ResolvedAuth, error) {
	authType := strings.ToLower(strings.TrimSpace(spec.Type))
	if authType == "" || authType == "none" {
		return ResolvedAuth{Type: "none"}, nil
	}

	res := ResolvedAuth{
		Type:       authType,
		Header:     strings.TrimSpace(spec.Header),
		QueryParam: strings.TrimSpace(spec.QueryParam),
		Prefix:     spec.Prefix,
	}
	if authType == "bearer" {
		if res.Header == "" {
			res.Header = "Authorization"
		}
		if res.Prefix == "" {
			res.Prefix = "Bearer "
		}
	}

	ref := strings.TrimSpace(spec.TokenRef)
	if ref == "" {
		if spec.Required {
			return res, fmt.Errorf("auth.token_ref is required")
		}
		return res, nil
	}

	var token string
	switch {
	case strings.HasPrefix(ref, "env://"):
		varName := strings.TrimSpace(strings.TrimPrefix(ref, "env://"))
		token = strings.TrimSpace(os.Getenv(varName))
		if token == "" && spec.Required {
			return res, fmt.Errorf("environment variable %s is not set", varName)
		}

	case strings.HasPrefix(ref, "auth://"):
		provID := strings.TrimSpace(strings.TrimPrefix(ref, "auth://"))
		if r.AuthStore != nil {
			if k, _, _, ok := r.AuthStore.ResolveCredential(provID); ok {
				token = strings.TrimSpace(k)
			}
		}
		if token == "" && spec.Required {
			return res, fmt.Errorf("no credential found for provider %q (run /login %s)", provID, provID)
		}

	case strings.HasPrefix(ref, "cmd://"):
		cmdStr := strings.TrimSpace(strings.TrimPrefix(ref, "cmd://"))
		parts := strings.Fields(cmdStr)
		if len(parts) == 0 {
			return res, fmt.Errorf("empty cmd:// auth reference")
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, parts[0], parts[1:]...).Output()
		if err != nil {
			if spec.Required {
				return res, fmt.Errorf("auth command failed: %w", err)
			}
			return res, nil
		}
		token = strings.TrimSpace(string(out))
		if token == "" && spec.Required {
			return res, fmt.Errorf("auth command returned empty token")
		}

	default:
		// 禁止明文密钥硬编码在 extension.yaml 中
		return res, fmt.Errorf("invalid token_ref %q: must use env://, auth://, or cmd:// (plaintext secrets are forbidden)", ref)
	}

	res.Token = token
	return res, nil
}

// SecretScrubber 将已解析的敏感凭证及其常见编码形态（原文/URL编码/Base64）统一替换为 [REDACTED]
type SecretScrubber struct {
	secrets []string
}

func NewSecretScrubber(tokens ...string) *SecretScrubber {
	sc := &SecretScrubber{}
	seen := make(map[string]bool)
	addWithEncodings := func(s string, minLen int) {
		s = strings.TrimSpace(s)
		if len(s) < minLen {
			return
		}
		for _, variant := range []string{s, url.QueryEscape(s), base64.StdEncoding.EncodeToString([]byte(s))} {
			if len(variant) >= minLen && !seen[variant] {
				seen[variant] = true
				sc.secrets = append(sc.secrets, variant)
			}
		}
	}
	for _, t := range tokens {
		addWithEncodings(t, 8)
	}
	sort.Slice(sc.secrets, func(i, j int) bool { return len(sc.secrets[i]) > len(sc.secrets[j]) })
	return sc
}

// NewSecretScrubberForAuth 构造包含鉴权 Token 原文、URL 编码态、Base64 编码态及敏感环境变量（含编码态）的脱敏器
func NewSecretScrubberForAuth(resolved ResolvedAuth, extraSecrets ...string) *SecretScrubber {
	sc := &SecretScrubber{}
	seen := make(map[string]bool)
	addWithEncodings := func(s string, minLen int) {
		s = strings.TrimSpace(s)
		if len(s) < minLen {
			return
		}
		for _, variant := range []string{s, url.QueryEscape(s), base64.StdEncoding.EncodeToString([]byte(s))} {
			if len(variant) >= minLen && !seen[variant] {
				seen[variant] = true
				sc.secrets = append(sc.secrets, variant)
			}
		}
	}
	if tok := strings.TrimSpace(resolved.Token); tok != "" {
		addWithEncodings(tok, 4)
	}
	for _, e := range extraSecrets {
		addWithEncodings(e, 8)
	}
	// 长串优先替换，避免短子串提前截断长编码串
	sort.Slice(sc.secrets, func(i, j int) bool { return len(sc.secrets[i]) > len(sc.secrets[j]) })
	return sc
}

func (s *SecretScrubber) Scrub(text string) string {
	if s == nil || len(s.secrets) == 0 || text == "" {
		return text
	}
	out := text
	for _, secret := range s.secrets {
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
	}
	return out
}
