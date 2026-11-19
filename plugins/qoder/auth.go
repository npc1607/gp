package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTokenTTL = 12 * time.Hour
	minTokenTTL     = time.Minute
	maxTokenTTL     = 24 * time.Hour
)

const (
	pathJobExchange = "/api/v1/jobToken/exchange"
	pathUserInfo    = "/api/v1/userinfo"
)

type realmEndpoint struct {
	Name    string
	OpenAPI string
	Gateway string
	UA      string
}

var defaultRealms = []realmEndpoint{
	{
		Name:    "intl",
		OpenAPI: "https://openapi.qoder.sh",
		Gateway: "https://api2.qoder.sh",
		UA:      "Qoder/1.1.75",
	},
	{
		Name:    "cn",
		OpenAPI: "https://openapi.qoder.com.cn",
		Gateway: "https://gateway.qoder.com.cn",
		UA:      "QoderWork/1.1.75",
	},
}

type exchangedAuth struct {
	Session *cosySession
	Gateway string
	Realm   string
	Expires time.Time
}

func exchangePAT(ctx context.Context, client *http.Client, pat string, realms []realmEndpoint) (*exchangedAuth, error) {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return nil, fmt.Errorf("qoder: empty PAT token")
	}
	if len(realms) == 0 {
		realms = defaultRealms
	}

	var errs []error
	for _, r := range realms {
		reqBody, _ := json.Marshal(map[string]any{"personal_token": pat})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.OpenAPI, "/")+pathJobExchange, bytes.NewReader(reqBody))
		if err != nil {
			errs = append(errs, fmt.Errorf("realm %s: %w", r.Name, err))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", r.UA)

		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("realm %s: %w", r.Name, err))
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// 注意：上游把两种失败都映射到 HTTP 400 BadRequest。
			//   带 rawMessage  → 请求体字段校验失败（personal_token 非空 string 之外的形状）
			//   不带 rawMessage → 请求体合法但 token 被拒（无效/过期的 PAT）
			// 所以这里拿到"裸"的 400 时，绝大多数情况是凭证本身有问题，而不是请求格式或 realm 选错。
			errs = append(errs, fmt.Errorf("realm %s jobToken exchange HTTP %d: %s", r.Name, resp.StatusCode, strings.TrimSpace(string(raw))))
			continue
		}

		var ex struct {
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
			ExpiresInMs  int64  `json:"expires_in_ms"`
		}
		if err := json.Unmarshal(raw, &ex); err != nil || ex.Token == "" {
			errs = append(errs, fmt.Errorf("realm %s invalid exchange response: %s", r.Name, strings.TrimSpace(string(raw))))
			continue
		}

		// Best-effort userinfo lookup
		uid, nickname, userType, orgID, orgName := "", "", "", "", ""
		if uReq, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.OpenAPI, "/")+pathUserInfo, nil); err == nil {
			uReq.Header.Set("Accept", "application/json")
			uReq.Header.Set("User-Agent", r.UA)
			uReq.Header.Set("Authorization", "Bearer "+ex.Token)
			if uResp, err := client.Do(uReq); err == nil {
				uRaw, _ := io.ReadAll(io.LimitReader(uResp.Body, 16*1024))
				_ = uResp.Body.Close()
				if uResp.StatusCode >= 200 && uResp.StatusCode < 300 {
					var ui struct {
						ID               string `json:"id"`
						Name             string `json:"name"`
						UserType         string `json:"user_type"`
						OrganizationID   string `json:"organization_id"`
						OrganizationName string `json:"organization_name"`
					}
					if json.Unmarshal(uRaw, &ui) == nil {
						uid = ui.ID
						nickname = ui.Name
						userType = ui.UserType
						orgID = ui.OrganizationID
						orgName = ui.OrganizationName
					}
				}
			}
		}
		if uid == "" {
			uid = "p-" + stableHexHash(pat, 6)
		}
		if nickname == "" {
			nickname = "qoder-user"
		}

		sess, err := newCosySession(uid, nickname, ex.Token, ex.RefreshToken, userType, orgID, orgName)
		if err != nil {
			return nil, err
		}
		return &exchangedAuth{
			Session: sess,
			Gateway: r.Gateway,
			Realm:   r.Name,
			Expires: resolveExpiry(time.Now(), ex.ExpiresIn, ex.ExpiresInMs),
		}, nil
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("no realm configured"))
	}
	// 聚合所有 realm 的错误：只保留最后一个会让排查时看不到 intl 的真实原因。
	err := fmt.Errorf("qoder: PAT exchange failed across %d realm(s): %w", len(realms), errors.Join(errs...))
	if !strings.HasPrefix(pat, "pt-") {
		err = fmt.Errorf("%w; hint: 原生流程需要 pt- 开头的 Personal Access Token，当前凭证疑似 API Key（上游对无效 token 一律返回 HTTP 400）", err)
	}
	return nil, err
}

// resolveExpiry 推断 jobToken 的失效时间。
//
// 上游未声明 expires_in 的单位，实测既可能是秒（3600）也可能是毫秒（3600000）。
// 取值小于 1e5 时按秒解释（1e5 秒约 27 小时，作为令牌 TTL 不合理），否则按毫秒解释，
// 于是 3600 和 3600000 都能得到 1 小时。结果再夹在 [1 分钟, 24 小时] 内：
// 宁可提前失效（后续 HTTP 401 会触发强制刷新），也不要拿过期 token 反复失败。
func resolveExpiry(now time.Time, expiresIn, expiresInMs int64) time.Time {
	var ttl time.Duration
	switch {
	case expiresInMs > 0:
		ttl = time.Duration(expiresInMs) * time.Millisecond
	case expiresIn > 0 && expiresIn < 100_000:
		ttl = time.Duration(expiresIn) * time.Second
	case expiresIn > 0:
		ttl = time.Duration(expiresIn) * time.Millisecond
	default:
		ttl = defaultTokenTTL
	}
	if ttl < minTokenTTL {
		ttl = minTokenTTL
	}
	if ttl > maxTokenTTL {
		ttl = maxTokenTTL
	}
	return now.Add(ttl)
}
