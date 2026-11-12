package resources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkillFile(t *testing.T) {
	tempDir := t.TempDir()
	skillPath := filepath.Join(tempDir, "SKILL.md")

	content := `---
name: go-testing
description: Guidelines for writing robust Go tests
author: agent-team
---
# Testing Rules
1. Always test race conditions.
2. Use t.TempDir for temporary files.
`
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test skill: %v", err)
	}

	skill, err := ParseSkillFile(skillPath)
	if err != nil {
		t.Fatalf("ParseSkillFile failed: %v", err)
	}

	if skill.Name != "go-testing" {
		t.Errorf("expected name go-testing, got %s", skill.Name)
	}
	if skill.Description != "Guidelines for writing robust Go tests" {
		t.Errorf("unexpected description: %s", skill.Description)
	}
	if !strings.Contains(skill.Instructions, "Always test race conditions.") {
		t.Errorf("expected instructions to contain race rule, got %s", skill.Instructions)
	}
	if skill.Metadata["author"] != "agent-team" {
		t.Errorf("expected metadata author=agent-team, got %v", skill.Metadata["author"])
	}
}

func TestResourceLoader_LoadSkillsAndOverride(t *testing.T) {
	homeDir := t.TempDir()
	cwdDir := t.TempDir()

	// 1. 创建全局 skill: linting
	globalSkillDir := filepath.Join(homeDir, ".gp", "skills", "linting")
	if err := os.MkdirAll(globalSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	globalSkillContent := `---
name: linting
description: Global linting guide
---
Run golangci-lint run
`
	_ = os.WriteFile(filepath.Join(globalSkillDir, "SKILL.md"), []byte(globalSkillContent), 0644)

	// 2. 创建项目级 skill: linting (覆盖全局) 和 formatting (项目专属)
	projectSkillDir := filepath.Join(cwdDir, ".gp", "skills", "linting")
	_ = os.MkdirAll(projectSkillDir, 0755)
	projectSkillContent := `---
name: linting
description: Project customized linting guide
---
Run custom linter ./bin/lint
`
	_ = os.WriteFile(filepath.Join(projectSkillDir, "SKILL.md"), []byte(projectSkillContent), 0644)

	cwdSkillsDir := filepath.Join(cwdDir, "skills", "formatting")
	_ = os.MkdirAll(cwdSkillsDir, 0755)
	cwdSkillsContent := `---
name: formatting
description: Code format rules
---
Use gofumpt -w
`
	_ = os.WriteFile(filepath.Join(cwdSkillsDir, "SKILL.md"), []byte(cwdSkillsContent), 0644)

	loader := NewResourceLoader(cwdDir, homeDir)
	skills, err := loader.LoadSkills()
	if err != nil {
		t.Fatalf("LoadSkills failed: %v", err)
	}

	if len(skills) != 2 {
		t.Fatalf("expected 2 merged skills, got %d", len(skills))
	}

	// 验证覆盖语义: linting 应为项目级的描述与正文
	var lintingSkill, formattingSkill *Skill
	for i := range skills {
		if skills[i].Name == "linting" {
			lintingSkill = &skills[i]
		}
		if skills[i].Name == "formatting" {
			formattingSkill = &skills[i]
		}
	}

	if lintingSkill == nil || formattingSkill == nil {
		t.Fatal("expected both linting and formatting skills to exist")
	}

	if lintingSkill.Description != "Project customized linting guide" {
		t.Errorf("expected project override description, got %s", lintingSkill.Description)
	}
	if !strings.Contains(lintingSkill.Instructions, "custom linter") {
		t.Errorf("expected project instructions, got %s", lintingSkill.Instructions)
	}
}

func TestResourceLoader_BuildSystemPrompt(t *testing.T) {
	cwdDir := t.TempDir()

	agentsContent := "Never commit directly to main branch."
	_ = os.WriteFile(filepath.Join(cwdDir, "AGENTS.md"), []byte(agentsContent), 0644)

	claudeContent := "Prefer concise answers."
	_ = os.WriteFile(filepath.Join(cwdDir, "CLAUDE.md"), []byte(claudeContent), 0644)

	skillDir := filepath.Join(cwdDir, "skills", "git-flow")
	_ = os.MkdirAll(skillDir, 0755)
	skillContent := `---
name: git-flow
description: Git branching strategy
---
Create feature branches from develop.
`
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)

	loader := NewResourceLoader(cwdDir, "")
	sysPrompt := loader.BuildSystemPrompt("You are an expert AI software engineer.")

	if !strings.Contains(sysPrompt, "You are an expert AI software engineer.") {
		t.Errorf("missing base prompt in: %s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, "# Engineering Rules & Guidelines") {
		t.Errorf("missing engineering guidelines section in: %s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, agentsContent) {
		t.Errorf("missing AGENTS.md content in: %s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, claudeContent) {
		t.Errorf("missing CLAUDE.md content in: %s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, "## Skill: git-flow") {
		t.Errorf("missing git-flow skill in: %s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, "Create feature branches from develop.") {
		t.Errorf("missing skill instructions in: %s", sysPrompt)
	}
}

func setupSkillFile(t *testing.T, dir, filename, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("setupSkillFile: mkdir %s error: %v", dir, err)
	}
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("setupSkillFile: write %s error: %v", filePath, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filePath)
	})
	return filePath
}

func TestResources_TC_RES_001_ProjectSkillOverrideGlobal(t *testing.T) {
	fn := "ResourceLoader.LoadSkills"
	homeDir := t.TempDir()
	cwdDir := t.TempDir()

	// 1. Global skill
	globalSkillDir := filepath.Join(homeDir, ".gp", "skills", "code-review")
	globalContent := `---
name: code-review
description: Global code review standards
tier: global
---
Review PRs globally.
`
	setupSkillFile(t, globalSkillDir, "SKILL.md", globalContent)

	// 2. Project skill overriding global
	projectSkillDir := filepath.Join(cwdDir, ".gp", "skills", "code-review")
	projectContent := `---
name: code-review
description: Project customized code review standards
tier: project
---
Review PRs with strict project linters.
`
	setupSkillFile(t, projectSkillDir, "SKILL.md", projectContent)

	// 3. Project-only unique skill
	projectOnlyDir := filepath.Join(cwdDir, "skills", "project-only")
	projectOnlyContent := `---
name: project-only
description: Project only skill
---
Project only instructions.
`
	setupSkillFile(t, projectOnlyDir, "SKILL.md", projectOnlyContent)

	loader := NewResourceLoader(cwdDir, homeDir)
	skills, err := loader.LoadSkills()
	if err != nil {
		t.Fatalf("%s(): got error %v, want nil", fn, err)
	}

	if got, want := len(skills), 2; got != want {
		t.Fatalf("%s(): got %d skills, want %d", fn, got, want)
	}

	var codeReviewSkill *Skill
	for i := range skills {
		if skills[i].Name == "code-review" {
			codeReviewSkill = &skills[i]
			break
		}
	}

	if codeReviewSkill == nil {
		t.Fatalf("%s(): skill 'code-review' not found in result", fn)
	}
	if got, want := codeReviewSkill.Description, "Project customized code review standards"; got != want {
		t.Fatalf("%s(): got description %q, want %q", fn, got, want)
	}
	if got, want := codeReviewSkill.Instructions, "Review PRs with strict project linters."; got != want {
		t.Fatalf("%s(): got instructions %q, want %q", fn, got, want)
	}
	if got, want := codeReviewSkill.Metadata["tier"], "project"; got != want {
		t.Fatalf("%s(): got metadata tier %v, want %v", fn, got, want)
	}
}

func TestResources_TC_RES_002_SystemPromptAssembly(t *testing.T) {
	fn := "ResourceLoader.BuildSystemPrompt"
	cwdDir := t.TempDir()

	agentsContent := "1. Always run tests before push."
	if err := os.WriteFile(filepath.Join(cwdDir, "AGENTS.md"), []byte(agentsContent), 0644); err != nil {
		t.Fatalf("write AGENTS.md: got error %v, want nil", err)
	}

	claudeContent := "2. Be concise and precise."
	if err := os.WriteFile(filepath.Join(cwdDir, "CLAUDE.md"), []byte(claudeContent), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: got error %v, want nil", err)
	}

	skillDir := filepath.Join(cwdDir, "skills", "git-workflow")
	skillContent := `---
name: git-workflow
description: Branching strategy guidelines
---
Use conventional commits format.
`
	setupSkillFile(t, skillDir, "SKILL.md", skillContent)

	loader := NewResourceLoader(cwdDir, "")
	basePrompt := "You are an autonomous AI coding assistant."
	sysPrompt := loader.BuildSystemPrompt(basePrompt)

	expectedSections := []struct {
		name     string
		contains string
	}{
		{"base prompt", basePrompt},
		{"guidelines header", "# Engineering Rules & Guidelines"},
		{"agents section header", "## Project Rules (AGENTS.md)"},
		{"agents content", agentsContent},
		{"claude section header", "## Project Guidelines (CLAUDE.md)"},
		{"claude content", claudeContent},
		{"skills section header", "# Available Domain Skills"},
		{"skill name header", "## Skill: git-workflow"},
		{"skill description", "**Description**: Branching strategy guidelines"},
		{"skill instructions", "Use conventional commits format."},
	}

	for _, tc := range expectedSections {
		if !strings.Contains(sysPrompt, tc.contains) {
			t.Fatalf("%s(base=%q): missing section %q (%q) in output:\n%s", fn, basePrompt, tc.name, tc.contains, sysPrompt)
		}
	}
}

func TestParseSkillFile_TableDriven(t *testing.T) {
	fn := "ParseSkillFile"

	tests := []struct {
		name        string
		input       string
		wantSkill   *Skill
		wantErr     bool
		description string
	}{
		{
			name:  "valid_frontmatter",
			input: "---\nname: s1\ndescription: d1\n---\nbody content",
			wantSkill: &Skill{
				Name:         "s1",
				Description:  "d1",
				Instructions: "body content",
			},
			wantErr:     false,
			description: "标准YAML Frontmatter解析",
		},
		{
			name:  "multiline_description",
			input: "---\nname: s2\ndescription: >\n  line1\n  line2\n---\nbody",
			wantSkill: &Skill{
				Name:         "s2",
				Description:  "line1 line2\n",
				Instructions: "body",
			},
			wantErr:     false,
			description: "多行折叠YAML描述解析",
		},
		{
			name:  "empty_frontmatter",
			input: "---\n---\nonly instructions body",
			wantSkill: &Skill{
				Name:         "default_fallback",
				Description:  "",
				Instructions: "only instructions body",
			},
			wantErr:     false,
			description: "空Frontmatter回退默认文件名",
		},
		{
			name:  "no_frontmatter",
			input: "pure markdown skill instruction without yaml markers",
			wantSkill: &Skill{
				Name:         "default_fallback",
				Description:  "",
				Instructions: "pure markdown skill instruction without yaml markers",
			},
			wantErr:     false,
			description: "无Frontmatter头回退纯文本",
		},
		{
			name:        "malformed_yaml",
			input:       "---\nname: [unclosed_array\n---\nbody",
			wantSkill:   nil,
			wantErr:     true,
			description: "语法损坏拦截报错",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caseDir := filepath.Join(t.TempDir(), "default_fallback")
			filePath := setupSkillFile(t, caseDir, "SKILL.md", tt.input)

			got, err := ParseSkillFile(filePath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("%s(input=%q): got error %v, wantErr %v", fn, tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got == nil {
				t.Fatalf("%s(input=%q): got nil skill, want non-nil", fn, tt.input)
			}
			if got, want := got.Name, tt.wantSkill.Name; got != want {
				t.Fatalf("%s(input=%q): got Name %q, want %q", fn, tt.input, got, want)
			}
			if got, want := got.Description, tt.wantSkill.Description; got != want {
				t.Fatalf("%s(input=%q): got Description %q, want %q", fn, tt.input, got, want)
			}
			if got, want := got.Instructions, tt.wantSkill.Instructions; got != want {
				t.Fatalf("%s(input=%q): got Instructions %q, want %q", fn, tt.input, got, want)
			}
		})
	}
}
