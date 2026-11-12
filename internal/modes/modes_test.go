package modes

import (
	"bytes"
	"context"
	"testing"

	"gp/internal/ai"
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
