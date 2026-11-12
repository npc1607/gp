package resources

// Skill 领域技能定义
type Skill struct {
	Name         string         `yaml:"name" json:"name"`
	Description  string         `yaml:"description" json:"description"`
	Instructions string         `yaml:"-" json:"instructions"`
	Path         string         `yaml:"-" json:"path"`
	Metadata     map[string]any `yaml:",inline" json:"metadata,omitempty"`
}

// ProjectContext 项目工程上下文
type ProjectContext struct {
	AgentsGuide string  `json:"agents_guide,omitempty"`
	ClaudeGuide string  `json:"claude_guide,omitempty"`
	Skills      []Skill `json:"skills,omitempty"`
}
