package extensions

import (
	"context"
	"fmt"
)

// guideAdapter 实现纯知识/SOP 型扩展（无外部进程、无动作，零开销）
type guideAdapter struct {
	spec *ExtensionSpec
}

func newGuideAdapter(spec *ExtensionSpec) Adapter {
	return &guideAdapter{spec: spec}
}

func (a *guideAdapter) Type() string { return "guide" }

func (a *guideAdapter) DiscoverActions(_ context.Context) ([]ActionDef, error) {
	return nil, nil
}

func (a *guideAdapter) Invoke(_ context.Context, req InvokeRequest) (*InvokeResult, error) {
	return &InvokeResult{
		IsError:   true,
		ErrorCode: ErrNotFound,
		Content:   formatCodedError(ErrNotFound, fmt.Sprintf("extension %q is guide-only and has no executable action %q; use ext_describe instead", a.spec.Name, req.Action)),
	}, nil
}

func (a *guideAdapter) HealthCheck(_ context.Context) error { return nil }
func (a *guideAdapter) Close() error                        { return nil }
