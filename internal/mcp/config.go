package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
			maps.Copy(merged.MCPServers, globalCfg.GetServers())
		}
	}

	// 2. 加载项目本地配置 (.agents/mcp.json, .gp/mcp.json，后者覆盖前者)
	if cwd != "" {
		for _, sub := range []string{".agents", ".gp"} {
			localPath := filepath.Join(cwd, sub, "mcp.json")
			localCfg, err := LoadConfigFile(localPath)
			if err != nil {
				return nil, err
			}
			if localCfg != nil {
				maps.Copy(merged.MCPServers, localCfg.GetServers())
			}
		}
	}

	return merged, nil
}

// AutoConnectAndRegister 扫描 MCP 配置，启动子进程并自动向 Registry 注册工具
func AutoConnectAndRegister(ctx context.Context, cwd, homeDir string, reg *tools.Registry) ([]*Client, error) {
	return AutoConnectAndRegisterWithDisabled(ctx, cwd, homeDir, reg, nil)
}

// AutoConnectAndRegisterWithDisabled 扫描 MCP 配置并跳过被禁用的服务，启动子进程并自动向 Registry 注册工具
func AutoConnectAndRegisterWithDisabled(ctx context.Context, cwd, homeDir string, reg *tools.Registry, disabledMCPs []string) ([]*Client, error) {
	if reg == nil {
		return nil, nil
	}

	cfg, err := DiscoverConfig(cwd, homeDir)
	if err != nil {
		return nil, fmt.Errorf("discover mcp configs: %w", err)
	}

	disabledSet := make(map[string]bool, len(disabledMCPs))
	for _, n := range disabledMCPs {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" {
			disabledSet[k] = true
		}
	}

	servers := cfg.GetServers()
	if len(servers) == 0 {
		return nil, nil
	}

	var activeClients []*Client
	for name, srv := range servers {
		if srv.Command == "" || srv.Disabled || disabledSet[strings.ToLower(strings.TrimSpace(name))] {
			continue
		}

		dir := srv.Cwd
		if dir == "" {
			dir = cwd
		}
		client, err := NewStdioClientWithDir(ctx, srv.Command, srv.Args, srv.Env, dir)
		if err != nil {
			// 忽略启动失败的单个 MCP 外部服务，保障核心 Agent 正常运行
			continue
		}
		client.ServerName = name

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
		client.ToolCount = len(toolsList)
		activeClients = append(activeClients, client)
	}

	return activeClients, nil
}

// InspectServers 汇总全局与项目级 MCP Server 的配置、启用状态及当前连接状态
func InspectServers(cwd, homeDir string, disabledMCPs []string, activeClients []*Client) ([]ServerStatus, error) {
	sources := make(map[string]string)
	merged := make(map[string]ServerConfig)

	if homeDir != "" {
		if gCfg, err := LoadConfigFile(filepath.Join(homeDir, ".gp", "mcp.json")); err != nil {
			return nil, err
		} else if gCfg != nil {
			for k, v := range gCfg.GetServers() {
				merged[k] = v
				sources[k] = "global"
			}
		}
	}
	if cwd != "" {
		for _, sub := range []string{".agents", ".gp"} {
			if lCfg, err := LoadConfigFile(filepath.Join(cwd, sub, "mcp.json")); err != nil {
				return nil, err
			} else if lCfg != nil {
				for k, v := range lCfg.GetServers() {
					merged[k] = v
					sources[k] = "project"
				}
			}
		}
	}

	disabledSet := make(map[string]bool, len(disabledMCPs))
	for _, n := range disabledMCPs {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" {
			disabledSet[k] = true
		}
	}

	connectedMap := make(map[string]int, len(activeClients))
	for _, c := range activeClients {
		if c != nil && c.ServerName != "" {
			connectedMap[c.ServerName] = c.ToolCount
		}
	}

	out := make([]ServerStatus, 0, len(merged))
	for name, srv := range merged {
		enabled := !srv.Disabled && !disabledSet[strings.ToLower(strings.TrimSpace(name))]
		tc, conn := connectedMap[name]
		out = append(out, ServerStatus{
			Name:      name,
			Command:   srv.Command,
			Args:      srv.Args,
			Source:    sources[name],
			Enabled:   enabled,
			Connected: conn,
			ToolCount: tc,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// RemoveServerConfig 从项目级或全局 mcp.json 中移除指定名称的 MCP Server 配置
func RemoveServerConfig(cwd, homeDir, name string) error {
	target := strings.TrimSpace(name)
	if target == "" {
		return fmt.Errorf("mcp server name cannot be empty")
	}
	var paths []string
	if cwd != "" {
		for _, sub := range []string{".gp", ".agents"} {
			paths = append(paths, filepath.Join(cwd, sub, "mcp.json"))
		}
	}
	if homeDir != "" {
		paths = append(paths, filepath.Join(homeDir, ".gp", "mcp.json"))
	}
	removed := false
	for _, p := range paths {
		cfg, err := LoadConfigFile(p)
		if err != nil || cfg == nil {
			continue
		}
		changed := false
		if _, ok := cfg.MCPServers[target]; ok {
			delete(cfg.MCPServers, target)
			changed = true
		}
		if _, ok := cfg.Servers[target]; ok {
			delete(cfg.Servers, target)
			changed = true
		}
		if changed {
			data, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil {
				return err
			}
			removed = true
		}
	}
	if !removed {
		return fmt.Errorf("mcp server %q not found in mcp.json", target)
	}
	return nil
}
