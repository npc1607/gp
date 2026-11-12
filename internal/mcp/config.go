package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gp/internal/tools"
)

// LoadConfigFile 读取并反序列化指定路径下的 mcp.json 配置文件
func LoadConfigFile(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mcp config (%s): %w", filePath, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal mcp config (%s): %w", filePath, err)
	}
	return &cfg, nil
}

// DiscoverConfig 按照就近覆盖原则合并全局 (~/.gp/mcp.json) 与工程级 (<cwd>/.gp/mcp.json) 配置
func DiscoverConfig(cwd, homeDir string) (*Config, error) {
	merged := &Config{
		MCPServers: make(map[string]ServerConfig),
	}

	// 1. 加载全局配置
	if homeDir != "" {
		globalPath := filepath.Join(homeDir, ".gp", "mcp.json")
		globalCfg, err := LoadConfigFile(globalPath)
		if err != nil {
			return nil, err
		}
		if globalCfg != nil {
			for k, v := range globalCfg.GetServers() {
				merged.MCPServers[k] = v
			}
		}
	}

	// 2. 加载项目本地配置 (覆盖同名配置)
	if cwd != "" {
		localPath := filepath.Join(cwd, ".gp", "mcp.json")
		localCfg, err := LoadConfigFile(localPath)
		if err != nil {
			return nil, err
		}
		if localCfg != nil {
			for k, v := range localCfg.GetServers() {
				merged.MCPServers[k] = v
			}
		}
	}

	return merged, nil
}

// AutoConnectAndRegister 扫描 MCP 配置，启动子进程并自动向 Registry 注册工具
func AutoConnectAndRegister(ctx context.Context, cwd, homeDir string, reg *tools.Registry) ([]*Client, error) {
	if reg == nil {
		return nil, nil
	}

	cfg, err := DiscoverConfig(cwd, homeDir)
	if err != nil {
		return nil, fmt.Errorf("discover mcp configs: %w", err)
	}

	servers := cfg.GetServers()
	if len(servers) == 0 {
		return nil, nil
	}

	var activeClients []*Client
	for name, srv := range servers {
		if srv.Command == "" {
			continue
		}

		client, err := NewStdioClient(ctx, srv.Command, srv.Args, srv.Env)
		if err != nil {
			// 忽略启动失败的单个 MCP 外部服务，保障核心 Agent 正常运行
			continue
		}

		// 握手
		if _, err := client.Initialize(ctx); err != nil {
			_ = client.Close()
			continue
		}

		// 发现工具并动态挂载
		toolsList, err := client.ListTools(ctx)
		if err != nil {
			_ = client.Close()
			continue
		}

		for _, def := range toolsList {
			mcpTool := NewMCPTool(name, client, def)
			_ = reg.Register(mcpTool)
		}

		activeClients = append(activeClients, client)
	}

	return activeClients, nil
}
