package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gp/internal/ai"
	_ "modernc.org/sqlite"
)

// Credential 表示持久化在 SQLite auth_credentials 表（及兼容 auth.json）中的凭证
type Credential struct {
	Type    string `json:"type"` // "api_key" | "oauth"
	Key     string `json:"key,omitempty"`
	Access  string `json:"access,omitempty"`
	Refresh string `json:"refresh,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// ProviderSpec 描述受支持登录认证与模型发现的 Provider 元信息
type ProviderSpec struct {
	ID          string
	Aliases     []string
	DisplayName string
	DefaultURL  string
	KeyHint     string
	EnvKeys     []string
	Models      []string
}

// RegisteredProviders 动态返回当前已挂载的全部 Provider 插件规格（例如 deepseek、qcorder）
func RegisteredProviders() []ProviderSpec {
	plugins := ai.ListProviderPlugins()
	out := make([]ProviderSpec, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, ProviderSpec{
			ID:          p.ID,
			Aliases:     p.Aliases,
			DisplayName: p.DisplayName,
			DefaultURL:  p.DefaultURL,
			KeyHint:     p.KeyHint,
			EnvKeys:     p.EnvKeys,
			Models:      p.DefaultModels,
		})
	}
	return out
}

// NormalizeProviderID 将别名（如 qoder -> qcorder）归一化为规范 Provider ID
func NormalizeProviderID(id string) string {
	return ai.NormalizePluginID(id)
}

// FindProviderSpec 根据 ID 或别名查找已注册的 Provider 插件规格
func FindProviderSpec(id string) (ProviderSpec, bool) {
	p, ok := ai.GetProviderPlugin(id)
	if !ok {
		return ProviderSpec{}, false
	}
	return ProviderSpec{
		ID:          p.ID,
		Aliases:     p.Aliases,
		DisplayName: p.DisplayName,
		DefaultURL:  p.DefaultURL,
		KeyHint:     p.KeyHint,
		EnvKeys:     p.EnvKeys,
		Models:      p.DefaultModels,
	}, true
}

// ModelEntry 表示 /model 选择器中展示的单个模型条目
type ModelEntry struct {
	Provider      string
	ID            string
	FullName      string // "<provider>/<id>"
	BaseURL       string
	Authenticated bool
	AuthSource    string // "sqlite" | "env" | "settings" | ""
}

// Store 基于 SQLite (auth_credentials & auth_models) 的高性能凭证与模型目录存储
type Store struct {
	mu       sync.RWMutex
	dbPath   string
	jsonPath string
	db       *sql.DB
}

// NewStore 使用指定 homeDir 创建 SQLite 凭证存储（~/.gp/auth.db）
func NewStore(homeDir string) *Store {
	if strings.TrimSpace(homeDir) == "" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			homeDir = h
		} else {
			homeDir = "."
		}
	}
	return &Store{
		dbPath:   filepath.Join(homeDir, ".gp", "auth.db"),
		jsonPath: filepath.Join(homeDir, ".gp", "auth.json"),
	}
}

// NewStoreWithPath 使用显式数据库路径创建凭证存储（兼容传入 .json 或 .db 路径）
func NewStoreWithPath(path string) *Store {
	dbPath := path
	jsonPath := ""
	if strings.HasSuffix(path, ".json") {
		dbPath = strings.TrimSuffix(path, ".json") + ".db"
		jsonPath = path
	}
	return &Store{
		dbPath:   dbPath,
		jsonPath: jsonPath,
	}
}

func (s *Store) Path() string {
	return s.dbPath
}

func (s *Store) ensureDB() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	dir := filepath.Dir(s.dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir auth dir: %w", err)
	}
	db, err := sql.Open("sqlite", s.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open auth.db: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 3000;",
		"PRAGMA synchronous = NORMAL;",
	} {
		_, _ = db.Exec(p)
	}
	schema := `
	CREATE TABLE IF NOT EXISTS auth_credentials (
		provider_id   TEXT PRIMARY KEY,
		cred_type     TEXT NOT NULL,
		api_key       TEXT NOT NULL DEFAULT '',
		access_token  TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '',
		expires_at    INTEGER NOT NULL DEFAULT 0,
		base_url      TEXT NOT NULL DEFAULT '',
		updated_at    TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS auth_models (
		provider_id  TEXT NOT NULL,
		model_id     TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT '',
		updated_at   TEXT NOT NULL,
		PRIMARY KEY (provider_id, model_id)
	);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init auth schema: %w", err)
	}
	_ = os.Chmod(s.dbPath, 0o600)
	s.db = db
	s.migrateLegacyJSONLocked(db)
	// 将历史拼写 qcorder 迁移为规范 ID qoder
	_, _ = db.Exec(`UPDATE OR IGNORE auth_credentials SET provider_id = 'qoder' WHERE provider_id = 'qcorder'`)
	_, _ = db.Exec(`DELETE FROM auth_credentials WHERE provider_id = 'qcorder'`)
	_, _ = db.Exec(`UPDATE OR IGNORE auth_models SET provider_id = 'qoder' WHERE provider_id = 'qcorder'`)
	_, _ = db.Exec(`DELETE FROM auth_models WHERE provider_id = 'qcorder'`)
	return db, nil
}

func (s *Store) migrateLegacyJSONLocked(db *sql.DB) {
	if s.jsonPath == "" {
		return
	}
	data, err := os.ReadFile(s.jsonPath)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return
	}
	var legacy map[string]Credential
	if err := json.Unmarshal(data, &legacy); err != nil || len(legacy) == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for k, c := range legacy {
		norm := NormalizeProviderID(k)
		if c.Type == "" {
			c.Type = "api_key"
		}
		_, _ = db.Exec(
			`INSERT OR IGNORE INTO auth_credentials(provider_id, cred_type, api_key, access_token, refresh_token, expires_at, base_url, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			norm, c.Type, c.Key, c.Access, c.Refresh, c.Expires, c.BaseURL, now,
		)
	}
}

// Close 关闭底层 SQLite 连接
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// LoadAll 从 SQLite auth_credentials 表读取全部凭证
func (s *Store) LoadAll() (map[string]Credential, error) {
	db, err := s.ensureDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT provider_id, cred_type, api_key, access_token, refresh_token, expires_at, base_url FROM auth_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]Credential)
	for rows.Next() {
		var id string
		var c Credential
		if err := rows.Scan(&id, &c.Type, &c.Key, &c.Access, &c.Refresh, &c.Expires, &c.BaseURL); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// Set 将指定 Provider 的凭证写入 SQLite auth_credentials 表
func (s *Store) Set(providerID string, cred Credential) error {
	norm := NormalizeProviderID(providerID)
	if norm == "" {
		return fmt.Errorf("provider id cannot be empty")
	}
	if cred.Type == "" {
		cred.Type = "api_key"
	}
	db, err := s.ensureDB()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(
		`INSERT INTO auth_credentials(provider_id, cred_type, api_key, access_token, refresh_token, expires_at, base_url, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider_id) DO UPDATE SET
		   cred_type=excluded.cred_type,
		   api_key=excluded.api_key,
		   access_token=excluded.access_token,
		   refresh_token=excluded.refresh_token,
		   expires_at=excluded.expires_at,
		   base_url=excluded.base_url,
		   updated_at=excluded.updated_at`,
		norm, cred.Type, cred.Key, cred.Access, cred.Refresh, cred.Expires, cred.BaseURL, now,
	)
	return err
}

// Remove 从 SQLite 删除指定 Provider 凭证与缓存模型（同时清理原始拼写与归一化别名）
func (s *Store) Remove(providerID string) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(providerID))
	norm := NormalizeProviderID(providerID)
	db, err := s.ensureDB()
	if err != nil {
		return false, err
	}
	candidates := map[string]bool{}
	if raw != "" {
		candidates[raw] = true
	}
	if norm != "" {
		candidates[norm] = true
	}
	if spec, ok := FindProviderSpec(norm); ok {
		for _, a := range spec.Aliases {
			if a != "" {
				candidates[strings.ToLower(strings.TrimSpace(a))] = true
			}
		}
	}
	var totalAffected int64
	for id := range candidates {
		if res, err := db.Exec(`DELETE FROM auth_credentials WHERE provider_id = ?`, id); err == nil {
			n, _ := res.RowsAffected()
			totalAffected += n
		}
		_, _ = db.Exec(`DELETE FROM auth_models WHERE provider_id = ?`, id)
	}
	return totalAffected > 0, nil
}

// Get 从 SQLite 读取指定 Provider 的凭证
func (s *Store) Get(providerID string) (Credential, bool) {
	norm := NormalizeProviderID(providerID)
	db, err := s.ensureDB()
	if err != nil {
		return Credential{}, false
	}
	var c Credential
	err = db.QueryRow(
		`SELECT cred_type, api_key, access_token, refresh_token, expires_at, base_url FROM auth_credentials WHERE provider_id = ?`,
		norm,
	).Scan(&c.Type, &c.Key, &c.Access, &c.Refresh, &c.Expires, &c.BaseURL)
	if err != nil {
		return Credential{}, false
	}
	return c, true
}

// SaveProviderModels 将拉取到的模型列表持久化到 SQLite auth_models 表
func (s *Store) SaveProviderModels(providerID string, modelIDs []string) error {
	norm := NormalizeProviderID(providerID)
	if norm == "" || len(modelIDs) == 0 {
		return nil
	}
	db, err := s.ensureDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM auth_models WHERE provider_id = ?`, norm); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range modelIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO auth_models(provider_id, model_id, display_name, updated_at) VALUES (?, ?, ?, ?)`,
			norm, id, id, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadProviderModels 从 SQLite auth_models 表瞬时读取缓存的模型列表
func (s *Store) LoadProviderModels(providerID string) []string {
	norm := NormalizeProviderID(providerID)
	db, err := s.ensureDB()
	if err != nil {
		return nil
	}
	rows, err := db.Query(`SELECT model_id FROM auth_models WHERE provider_id = ? ORDER BY rowid ASC`, norm)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ResolveCredential 按 SQLite auth_credentials -> 环境变量 解析 Provider 凭证
func (s *Store) ResolveCredential(providerID string) (apiKey, baseURL, source string, ok bool) {
	norm := NormalizeProviderID(providerID)
	spec, hasSpec := FindProviderSpec(norm)

	if s != nil {
		if cred, found := s.Get(norm); found {
			token := cred.Key
			if token == "" {
				token = cred.Access
			}
			if token != "" {
				url := cred.BaseURL
				if url == "" && hasSpec {
					url = spec.DefaultURL
				}
				return token, url, "sqlite", true
			}
		}
	}

	if hasSpec {
		defaultURL := spec.DefaultURL
		if norm == "qoder" {
			if envURL := strings.TrimSpace(os.Getenv("QODER_API_BASE_URL")); envURL != "" {
				defaultURL = envURL
			} else if envURL := strings.TrimSpace(os.Getenv("QCORDER_BASE_URL")); envURL != "" {
				defaultURL = envURL
			}
		}
		for _, envName := range spec.EnvKeys {
			if val := strings.TrimSpace(os.Getenv(envName)); val != "" {
				return val, defaultURL, "env", true
			}
		}
	}
	return "", "", "", false
}

// InferProviderFromModel 根据模型名称推断所属 Provider
func InferProviderFromModel(model string) string {
	return ai.InferPluginByModel(model)
}

// ListModels 零网络阻塞地从 SQLite 缓存与已挂载插件目录汇总可用模型（打开 /model 毫秒级响应）
func (s *Store) ListModels(currentModel, activeBaseURL, activeAPIKey string) []ModelEntry {
	var entries []ModelEntry
	seen := make(map[string]bool)

	for _, spec := range RegisteredProviders() {
		key, baseURL, src, ok := s.ResolveCredential(spec.ID)
		if !ok && activeAPIKey != "" && activeBaseURL != "" {
			if (spec.ID == "deepseek" && strings.Contains(strings.ToLower(activeBaseURL), "deepseek")) ||
				(spec.ID == "qoder" && strings.Contains(strings.ToLower(activeBaseURL), "qoder")) {
				key = activeAPIKey
				baseURL = activeBaseURL
				src = "env"
				ok = true
			}
		}
		_ = key
		if baseURL == "" {
			baseURL = spec.DefaultURL
		}

		// 优先使用 SQLite 缓存的线上模型列表，并与内置兜底合并去重
		var merged []string
		modelSeen := make(map[string]bool)
		if s != nil {
			for _, id := range s.LoadProviderModels(spec.ID) {
				if !modelSeen[id] {
					modelSeen[id] = true
					merged = append(merged, id)
				}
			}
		}
		for _, id := range spec.Models {
			if !modelSeen[id] {
				modelSeen[id] = true
				merged = append(merged, id)
			}
		}

		for _, mID := range merged {
			full := spec.ID + "/" + mID
			seen[full] = true
			seen[mID] = true
			entries = append(entries, ModelEntry{
				Provider:      spec.ID,
				ID:            mID,
				FullName:      full,
				BaseURL:       baseURL,
				Authenticated: ok,
				AuthSource:    src,
			})
		}
	}

	if cm := strings.TrimSpace(currentModel); cm != "" && !seen[cm] {
		prov := InferProviderFromModel(cm)
		if prov == "" {
			prov = "custom"
		}
		src := ""
		if activeAPIKey != "" {
			src = "settings"
		}
		entries = append([]ModelEntry{{
			Provider:      prov,
			ID:            cm,
			FullName:      cm,
			BaseURL:       activeBaseURL,
			Authenticated: activeAPIKey != "",
			AuthSource:    src,
		}}, entries...)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Authenticated != entries[j].Authenticated {
			return entries[i].Authenticated
		}
		return false
	})
	return entries
}

// RefreshProviderModels 异步/按需向 Provider 请求线上模型目录并写入 SQLite auth_models 表
func (s *Store) RefreshProviderModels(providerID string) (int, error) {
	key, baseURL, _, ok := s.ResolveCredential(providerID)
	if !ok || key == "" {
		return 0, fmt.Errorf("provider %s not authenticated", providerID)
	}
	norm := NormalizeProviderID(providerID)
	if plugin, hasPlugin := ai.GetProviderPlugin(norm); hasPlugin && plugin.FetchModels != nil {
		ids, err := plugin.FetchModels(key, baseURL)
		if err == nil && len(ids) > 0 {
			if err := s.SaveProviderModels(norm, ids); err != nil {
				return 0, err
			}
			return len(ids), nil
		}
	}

	ep := strings.TrimRight(baseURL, "/") + "/models"
	client := &http.Client{Timeout: 4 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data []struct {
			ID        string `json:"id"`
			IsEnabled *bool  `json:"is_enabled,omitempty"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || len(body.Data) == 0 {
		return 0, fmt.Errorf("no models returned from %s", ep)
	}
	var ids []string
	for _, item := range body.Data {
		if item.ID == "" || (item.IsEnabled != nil && !*item.IsEnabled) {
			continue
		}
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("no enabled models returned from %s", ep)
	}
	if err := s.SaveProviderModels(norm, ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}
