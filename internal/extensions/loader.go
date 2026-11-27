package extensions

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gp/internal/core/resources"
	"gp/internal/mcp"

	"gopkg.in/yaml.v3"
)

// LoadOptions 控制 Extension 扫描发现与旧生态桥接行为
type LoadOptions struct {
	Cwd                string
	HomeDir            string
	ExtraDirs          []string
	DisabledExtensions []string
	BridgeLegacySkills []resources.Skill
	BridgeLegacyMCP    *mcp.Config
	DisabledMCPs       []string
}

// LoadedExtension 表示一个被扫描发现的扩展（包含解析失败的容错状态，保证故障隔离）
type LoadedExtension struct {
	Spec      *ExtensionSpec
	Enabled   bool
	LoadError string
}

// ParseManifestFile 解析单个 extension.yaml 文件并加载关联 GUIDE.md（执行路径沙箱校验）
func ParseManifestFile(filePath, source string) (*ExtensionSpec, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read extension manifest (%s): %w", filePath, err)
	}
	var spec ExtensionSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse extension manifest (%s): %w", filePath, err)
	}
	spec.Dir = filepath.Dir(filePath)
	spec.FilePath = filePath
	spec.Source = source
	if spec.Name == "" {
		spec.Name = strings.ToLower(filepath.Base(spec.Dir))
	}
	if spec.APIVersion == "" {
		spec.APIVersion = APIVersionV1
	}
	spec.ExpandSpecEnv()
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	if strings.TrimSpace(spec.Guide) != "" {
		guidePath, pErr := safeJoinUnderDir(spec.Dir, spec.Guide)
		if pErr != nil {
			return nil, fmt.Errorf("extension %q invalid guide path: %w", spec.Name, pErr)
		}
		if gBytes, gErr := os.ReadFile(guidePath); gErr == nil {
			spec.GuideText = strings.TrimSpace(string(gBytes))
		}
	}
	return &spec, nil
}

// DiscoverExtensions 按优先级扫描全局、自定义与项目目录，并可选桥接旧 Skill 与 MCP 配置
func DiscoverExtensions(opts LoadOptions) []LoadedExtension {
	disabledSet := toLowerSet(opts.DisabledExtensions)
	disabledMCPSet := toLowerSet(opts.DisabledMCPs)
	byName := make(map[string]LoadedExtension)

	// 1. 先桥接旧生态（优先级最低，同名 extension.yaml 可无痛覆盖）
	for _, sk := range opts.BridgeLegacySkills {
		name := normalizeLegacyName(sk.Name)
		if name == "" {
			continue
		}
		en := sk.Enabled && !disabledSet[strings.ToLower(name)]
		desc := strings.TrimSpace(sk.Description)
		if desc == "" {
			desc = "Legacy skill guide: " + sk.Name
		}
		spec := &ExtensionSpec{
			APIVersion:  APIVersionV1,
			Name:        name,
			Description: desc,
			Adapter:     AdapterSpec{Type: "guide"},
			Source:      "legacy-skill",
			FilePath:    sk.Path,
			Dir:         filepath.Dir(sk.Path),
			GuideText:   strings.TrimSpace(sk.Instructions),
		}
		byName[name] = LoadedExtension{Spec: spec, Enabled: en}
	}

	if opts.BridgeLegacyMCP != nil {
		for srvName, srv := range opts.BridgeLegacyMCP.GetServers() {
			if strings.TrimSpace(srv.Command) == "" {
				continue
			}
			name := normalizeLegacyName(srvName)
			if name == "" {
				continue
			}
			en := !srv.Disabled && !disabledMCPSet[strings.ToLower(srvName)] && !disabledSet[strings.ToLower(name)]
			envClone := make(map[string]string, len(srv.Env))
			for k, v := range srv.Env {
				envClone[k] = v
			}
			spec := &ExtensionSpec{
				APIVersion:  APIVersionV1,
				Name:        name,
				Description: fmt.Sprintf("MCP server (%s)", srvName),
				Adapter: AdapterSpec{
					Type:      "mcp",
					Transport: "stdio",
					Command:   srv.Command,
					Args:      append([]string(nil), srv.Args...),
					Env:       envClone,
					Cwd:       srv.Cwd,
				},
				WorkspaceCwd: opts.Cwd,
				Source:       "legacy-mcp",
			}
			spec.ExpandSpecEnv()
			byName[name] = LoadedExtension{Spec: spec, Enabled: en}
		}
	}

	// 2. 按优先级扫描 extension.yaml（后扫目录覆盖先扫同名扩展）
	type scanRoot struct {
		dir    string
		source string
	}
	var roots []scanRoot
	if opts.HomeDir != "" {
		roots = append(roots,
			scanRoot{dir: filepath.Join(opts.HomeDir, ".agents", "extensions"), source: "global"},
			scanRoot{dir: filepath.Join(opts.HomeDir, ".gp", "extensions"), source: "global"},
		)
	}
	for _, d := range opts.ExtraDirs {
		if td := strings.TrimSpace(d); td != "" {
			roots = append(roots, scanRoot{dir: td, source: "custom"})
		}
	}
	if opts.Cwd != "" {
		roots = append(roots,
			scanRoot{dir: filepath.Join(opts.Cwd, ".agents", "extensions"), source: "project"},
			scanRoot{dir: filepath.Join(opts.Cwd, ".gp", "extensions"), source: "project"},
		)
	}

	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			manifest := filepath.Join(root.dir, ent.Name(), "extension.yaml")
			if _, stErr := os.Stat(manifest); stErr != nil {
				manifestYML := filepath.Join(root.dir, ent.Name(), "extension.yml")
				if _, stErr2 := os.Stat(manifestYML); stErr2 != nil {
					continue
				}
				manifest = manifestYML
			}
			spec, parseErr := ParseManifestFile(manifest, root.source)
			if parseErr != nil {
				fallbackName := normalizeLegacyName(ent.Name())
				if fallbackName == "" {
					fallbackName = ent.Name()
				}
				byName[fallbackName] = LoadedExtension{
					Spec: &ExtensionSpec{
						Name:        fallbackName,
						Description: "(manifest error)",
						Source:      root.source,
						FilePath:    manifest,
						Dir:         filepath.Dir(manifest),
					},
					Enabled:   false,
					LoadError: parseErr.Error(),
				}
				continue
			}
			spec.WorkspaceCwd = opts.Cwd
			en := spec.IsDefaultEnabled() && !disabledSet[strings.ToLower(spec.Name)]
			byName[spec.Name] = LoadedExtension{
				Spec:    spec,
				Enabled: en,
			}
		}
	}

	// 按 name 升序确定性返回，保护 Prompt Cache 前缀稳定
	names := make([]string, 0, len(byName))
	for k := range byName {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]LoadedExtension, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out
}

func normalizeLegacyName(raw string) string {
	low := strings.ToLower(strings.TrimSpace(raw))
	var sb strings.Builder
	for _, r := range low {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else if r == ' ' || r == '/' || r == '.' {
			sb.WriteByte('-')
		}
	}
	out := strings.Trim(sb.String(), "-_")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
