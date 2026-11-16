package tui

// UIConfig 统一配置 pi-tidy 与 pi-zentui 风格的视觉与交互参数
type UIConfig struct {
	Icons bool

	// pi-tidy: User message & Reply & Turn folding
	UserMessageStyle   string   // "frame" | "zentui-framed" | "rail" | "rules" | "off"
	UserMessageAlign   string   // "left" | "right"
	UserMessageTime    bool
	CollapseTurns      string   // "afterRun" | "live" | "off"
	FinalReplyStyle    string   // "frame" | "rail" | "rules" | "off"
	MessageHeaderStyle string   // "inline" | "split"
	GroupTools         bool
	GroupOpen          bool
	AlwaysShowTools    []string // default: ["edit", "write"]
	BashResultLines    int      // default: 1
	WriteLines         int      // default: 10
	DiffLines          int      // default: 20
	SplitDiff          bool
	SplitDiffMinWidth  int      // default: 72
	CodeBlocks         bool
	HexSwatches        bool
	HighlightBash      bool

	// pi-zentui: Editor, Working Line, Footer
	EditorStyle    string // "opencode" | "accent-rail" | "minimalist"
	FooterStyle    string // "starship" | "hidden"
	WorkingSpinner string // "braille" | "star-bloom" | "pinwheel" | "claude-inspired" | "pulse"
	ShowThinking   bool
}

// DefaultUIConfig 返回完美融合 pi-tidy 与 pi-zentui 默认值的配置
func DefaultUIConfig() UIConfig {
	return UIConfig{
		Icons:              true,
		UserMessageStyle:   "frame",
		UserMessageAlign:   "left",
		UserMessageTime:    true,
		CollapseTurns:      "afterRun",
		FinalReplyStyle:    "frame",
		MessageHeaderStyle: "inline",
		GroupTools:         true,
		GroupOpen:          false,
		AlwaysShowTools:    []string{"edit", "write"},
		BashResultLines:    1,
		WriteLines:         10,
		DiffLines:          20,
		SplitDiff:          true,
		SplitDiffMinWidth:  72,
		CodeBlocks:         true,
		HexSwatches:        true,
		HighlightBash:      true,
		EditorStyle:        "opencode",
		FooterStyle:        "starship",
		WorkingSpinner:     "claude-inspired",
		ShowThinking:       true,
	}
}

// IsAlwaysShownTool 判断工具是否始终以独立圆角框展示（不折叠进工具组）
func (c UIConfig) IsAlwaysShownTool(name string) bool {
	for _, t := range c.AlwaysShowTools {
		if t == name {
			return true
		}
	}
	return false
}
