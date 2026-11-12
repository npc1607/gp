package modes

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gp/internal/ai"
	"gp/internal/modes/interactive"
	"gp/internal/modes/print"
)

type mockProvider struct {
	chunks []ai.StreamChunk
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func TestPrintRunner_RunInterface(t *testing.T) {
	fn := "modes.PrintRunner.Run"
	var buf bytes.Buffer
	mock := &mockProvider{
		chunks: []ai.StreamChunk{
			{DeltaText: "hello from modes runner"},
		},
	}

	var runner Runner = &PrintRunner{
		Opts: print.RunnerOptions{
			Writer:   &buf,
			Provider: mock,
		},
	}

	prompt := "modes test"
	err := runner.Run(context.Background(), prompt)
	if err != nil {
		t.Fatalf("%s(prompt=%q): got error %v, want nil", fn, prompt, err)
	}

	expected := "hello from modes runner\n"
	if got, want := buf.String(), expected; got != want {
		t.Fatalf("%s(prompt=%q): got output %q, want %q", fn, prompt, got, want)
	}
}

func TestResumeRunner_NoUnfinished(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockProvider{}

	runner := &ResumeRunner{
		Opts: ResumeRunnerOptions{
			DBPath:   ":memory:",
			Writer:   &buf,
			Provider: mock,
		},
	}

	err := runner.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("expected nil error on empty unfinished tasks, got %v", err)
	}
	if !strings.Contains(buf.String(), "No unfinished tasks") {
		t.Fatalf("expected 'No unfinished tasks' message, got %q", buf.String())
	}
}

func TestInteractiveRunner_Contract(t *testing.T) {
	mock := &mockProvider{
		chunks: []ai.StreamChunk{
			{DeltaText: "hello from interactive runner"},
		},
	}
	var runner Runner = &InteractiveRunner{
		Opts: interactive.RunnerOptions{
			Provider: mock,
			Headless: true,
		},
	}

	err := runner.Run(context.Background(), "interactive test")
	if err != nil {
		t.Fatalf("expected nil error from interactive runner in headless mode, got: %v", err)
	}
}
