package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gp/internal/tools"
)

func TestBashToolEmitsProgressAndStalledEvents(t *testing.T) {
	origDelay := bashProgressInitialDelay
	origInterval := bashProgressInterval
	origStall := bashStallThreshold
	bashProgressInitialDelay = 20 * time.Millisecond
	bashProgressInterval = 25 * time.Millisecond
	bashStallThreshold = 45 * time.Millisecond
	defer func() {
		bashProgressInitialDelay = origDelay
		bashProgressInterval = origInterval
		bashStallThreshold = origStall
	}()

	evCh := make(chan tools.ToolEvent, 16)
	bt := &BashTool{}
	ctx := &tools.ToolContext{
		Context:   context.Background(),
		Cwd:       t.TempDir(),
		EventChan: evCh,
	}

	rawArgs, _ := json.Marshal(map[string]string{
		"command": "echo 'step-1 output'; sleep 0.14",
	})
	res, err := bt.Execute(ctx, rawArgs)
	if err != nil || res.IsError {
		t.Fatalf("unexpected bash error: err=%v res=%+v", err, res)
	}
	close(evCh)

	var gotUpdate, gotStalled bool
	for ev := range evCh {
		if ev.ToolName != "bash" {
			t.Fatalf("unexpected tool name %q", ev.ToolName)
		}
		if strings.Contains(ev.Action, "step-1 output") || strings.Contains(ev.Action, "输出更新") {
			gotUpdate = true
		}
		if ev.Stalled {
			gotStalled = true
		}
	}
	if !gotUpdate {
		t.Fatal("expected at least one bash output update progress event")
	}
	if !gotStalled {
		t.Fatal("expected stalled=true progress event after silent threshold")
	}
}
