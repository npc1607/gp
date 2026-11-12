package builtin

import (
	"fmt"

	"gp/internal/tools"
)

// RegisterAll 将所有内置工具注册到指定的注册中心
func RegisterAll(r *tools.Registry) error {
	builtins := []tools.Tool{
		&ReadTool{},
		&WriteTool{},
		&EditTool{},
		&BashTool{},
	}

	for _, t := range builtins {
		if err := r.Register(t); err != nil {
			return fmt.Errorf("register builtin tool %q: %w", t.Name(), err)
		}
	}
	return nil
}

// NewDefaultRegistry 创建并初始化一个包含全部内置工具的默认注册中心
func NewDefaultRegistry() (*tools.Registry, error) {
	r := tools.NewRegistry()
	if err := RegisterAll(r); err != nil {
		return nil, err
	}
	return r, nil
}
