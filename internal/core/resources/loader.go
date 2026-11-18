package resources

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ResourceLoader 负责扫描加载 Skills、AGENTS.md、CLAUDE.md 等工程上下文资源
type ResourceLoader struct {
	cwd                string
	homeDir            string
	extraSkillDirs     []string
	disabledSkills     map[string]bool
	unregisteredSkills map[string]bool
}

// NewResourceLoader 构造资源加载器
func NewResourceLoader(cwd, homeDir string) *ResourceLoader {
	return &ResourceLoader{
		cwd:                cwd,
		homeDir:            homeDir,
		disabledSkills:     make(map[string]bool),
		unregisteredSkills: make(map[string]bool),
	}
}

// SetExtraSkillDirs 配置额外的自定义 Skill 扫描目录
func (r *ResourceLoader) SetExtraSkillDirs(dirs []string) {
	r.extraSkillDirs = append([]string(nil), dirs...)
}

// SetDisabledSkills 配置被禁用的 Skill 名称列表（大小写不敏感）
func (r *ResourceLoader) SetDisabledSkills(names []string) {
	r.disabledSkills = make(map[string]bool, len(names))
	for _, n := range names {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" {
			r.disabledSkills[k] = true
		}
	}
}

// SetUnregisteredSkills 配置已取消注册的 Skill 名称列表（从展示与加载中排除）
func (r *ResourceLoader) SetUnregisteredSkills(names []string) {
	r.unregisteredSkills = make(map[string]bool, len(names))
	for _, n := range names {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" {
			r.unregisteredSkills[k] = true
		}
	}
}

// ParseSkillFile 解析单个 SKILL.md 文件 (支持 YAML Frontmatter 与 Markdown 正文提取)
func ParseSkillFile(filePath string) (*Skill, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read skill file (%s): %w", filePath, err)
	}

	content := string(data)
	normalized := strings.ReplaceAll(content, "\r\n", "\n")

	skill := &Skill{
		Path: filePath,
	}

	trimmed := strings.TrimSpace(normalized)
	if strings.HasPrefix(trimmed, "---") {
		// 寻找第二个 ---
		rest := trimmed[3:]
		before, after, ok := strings.Cut(rest, "\n---")
		if ok {
			frontmatter := before + "\n"
			body := after

			if yamlErr := yaml.Unmarshal([]byte(frontmatter), skill); yamlErr != nil {
				return nil, fmt.Errorf("parse yaml frontmatter (%s): %w", filePath, yamlErr)
			}
			skill.Instructions = strings.TrimSpace(body)
		} else {
			// 没有闭合标记，作为纯正文处理
			skill.Instructions = trimmed
		}
	} else {
		skill.Instructions = trimmed
	}

	// 容错：如果未声明 name，以父目录名或文件名兜底
	if skill.Name == "" {
		dir := filepath.Base(filepath.Dir(filePath))
		if dir != "." && dir != "/" && dir != "skills" && dir != ".gp" {
			skill.Name = dir
		} else {
			base := filepath.Base(filePath)
			skill.Name = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}

	return skill, nil
}

// LoadSkills 发现并加载全局 (~/.gp/skills) 与项目级 (<cwd>/.gp/skills, <cwd>/skills) 的全部技能，支持就近覆盖
func (r *ResourceLoader) LoadSkills() ([]Skill, error) {
	skillsMap := make(map[string]Skill)

	// 1. 扫描全局技能 (~/.gp/skills, ~/.agents/skills)
	if r.homeDir != "" {
		r.scanSkillsDir(filepath.Join(r.homeDir, ".gp", "skills"), "global", skillsMap)
		r.scanSkillsDir(filepath.Join(r.homeDir, ".agents", "skills"), "global", skillsMap)
	}

	// 2. 扫描项目级技能 (覆盖同名全局技能)
	if r.cwd != "" {
		r.scanSkillsDir(filepath.Join(r.cwd, ".gp", "skills"), "project", skillsMap)
		r.scanSkillsDir(filepath.Join(r.cwd, ".agents", "skills"), "project", skillsMap)
		r.scanSkillsDir(filepath.Join(r.cwd, "skills"), "project", skillsMap)
	}

	// 3. 扫描用户配置的额外技能目录
	for _, dir := range r.extraSkillDirs {
		if strings.TrimSpace(dir) != "" {
			r.scanSkillsDir(dir, "custom", skillsMap)
		}
	}

	var result []Skill
	for _, s := range skillsMap {
		key := strings.ToLower(strings.TrimSpace(s.Name))
		if r.unregisteredSkills[key] {
			continue
		}
		s.Enabled = !r.disabledSkills[key]
		result = append(result, s)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

func (r *ResourceLoader) scanSkillsDir(dir, source string, targetMap map[string]Skill) {
	if _, err := os.Stat(dir); err != nil {
		return
	}

	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		lowerName := strings.ToLower(d.Name())
		if lowerName == "readme.md" || lowerName == "usage.md" || lowerName == "license.md" {
			return nil
		}
		if lowerName == "skill.md" || strings.HasSuffix(lowerName, ".md") {
			skill, parseErr := ParseSkillFile(path)
			if parseErr == nil && skill != nil && skill.Name != "" && !strings.EqualFold(skill.Name, "readme") {
				skill.Source = source
				targetMap[skill.Name] = *skill
			}
		}
		return nil
	})
}

// LoadProjectContext 加载项目级工程规范及技能
func (r *ResourceLoader) LoadProjectContext() (*ProjectContext, error) {
	ctx := &ProjectContext{}

	if r.cwd != "" {
		agentsPath := filepath.Join(r.cwd, "AGENTS.md")
		if data, err := os.ReadFile(agentsPath); err == nil {
			ctx.AgentsGuide = strings.TrimSpace(string(data))
		}

		claudePath := filepath.Join(r.cwd, "CLAUDE.md")
		if data, err := os.ReadFile(claudePath); err == nil {
			ctx.ClaudeGuide = strings.TrimSpace(string(data))
		}
	}

	skills, err := r.LoadSkills()
	if err == nil {
		ctx.Skills = skills
	}

	return ctx, nil
}

// BuildSystemPrompt 组装完整的 System Prompt，将工程规则与 Skills 注入提示词上下文
func (r *ResourceLoader) BuildSystemPrompt(basePrompt string) string {
	var sb strings.Builder

	if trimmed := strings.TrimSpace(basePrompt); trimmed != "" {
		sb.WriteString(trimmed)
		sb.WriteString("\n\n")
	}

	ctx, err := r.LoadProjectContext()
	if err == nil && ctx != nil {
		hasGuidelines := ctx.AgentsGuide != "" || ctx.ClaudeGuide != ""
		if hasGuidelines {
			sb.WriteString("# Engineering Rules & Guidelines\n\n")
			if ctx.AgentsGuide != "" {
				sb.WriteString("## Project Rules (AGENTS.md)\n")
				sb.WriteString(ctx.AgentsGuide)
				sb.WriteString("\n\n")
			}
			if ctx.ClaudeGuide != "" {
				sb.WriteString("## Project Guidelines (CLAUDE.md)\n")
				sb.WriteString(ctx.ClaudeGuide)
				sb.WriteString("\n\n")
			}
		}

		if len(ctx.Skills) > 0 {
			var enabledSkills []Skill
			for _, s := range ctx.Skills {
				if s.Enabled {
					enabledSkills = append(enabledSkills, s)
				}
			}
			if len(enabledSkills) > 0 {
				sb.WriteString("# Available Domain Skills\n\n")
				for _, s := range enabledSkills {
					fmt.Fprintf(&sb, "## Skill: %s\n", s.Name)
					if s.Description != "" {
						fmt.Fprintf(&sb, "**Description**: %s\n\n", s.Description)
					}
					if s.Instructions != "" {
						sb.WriteString(s.Instructions)
						sb.WriteString("\n\n")
					}
				}
			}
		}
	}

	return strings.TrimSpace(sb.String())
}
