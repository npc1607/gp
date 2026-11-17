package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

	var lastErr error
	for _, r := range realms {
		reqBody, _ := json.Marshal(map[string]any{"personal_token": pat})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.OpenAPI, "/")+pathJobExchange, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", r.UA)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("realm %s jobToken exchange HTTP %d: %s", r.Name, resp.StatusCode, strings.TrimSpace(string(raw)))
			continue
		}

		var ex struct {
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(raw, &ex); err != nil || ex.Token == "" {
			lastErr = fmt.Errorf("realm %s invalid exchange response: %s", r.Name, strings.TrimSpace(string(raw)))
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
		exp := time.Now().Add(12 * time.Hour)
		if ex.ExpiresIn > 0 {
			exp = time.Now().Add(time.Duration(ex.ExpiresIn) * time.Millisecond)
		}
		return &exchangedAuth{
			Session: sess,
			Gateway: r.Gateway,
			Realm:   r.Name,
			Expires: exp,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("qoder: PAT exchange failed")
	}
	return nil, lastErr
}
