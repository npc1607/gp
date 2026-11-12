package js

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gp/internal/tools"
)

// LoadExtensions 扫描全局与工程目录下的 .js 扩展脚本并加载注入 Registry
func LoadExtensions(cwd, homeDir string, reg *tools.Registry) ([]*JSTool, error) {
	engine := NewEngine(reg)

	var scanDirs []string
	if homeDir != "" {
		scanDirs = append(scanDirs, filepath.Join(homeDir, ".gp", "extensions"))
	}
	if cwd != "" {
		scanDirs = append(scanDirs, filepath.Join(cwd, ".gp", "extensions"))
	}

	var loadedFiles []string
	for _, dir := range scanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录不存在或不可读时静默跳过
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if strings.HasSuffix(strings.ToLower(entry.Name()), ".js") {
				loadedFiles = append(loadedFiles, filepath.Join(dir, entry.Name()))
			}
		}
	}

	// 保证确定性加载顺序
	sort.Strings(loadedFiles)

	for _, file := range loadedFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if err := engine.ExecuteScript(string(content), file); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 加载 JS 扩展 (%s) 失败: %v\n", file, err)
		}
	}

	return engine.Tools(), nil
}
