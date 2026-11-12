package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gp/internal/tools"
)

// TC-PEER-01: AskPeer点对点协同问答
func TestAskPeerTool_TC_PEER_01_PeerCollaborativeQuery(t *testing.T) {
	fn := "AskPeerTool.Execute"
	var handlerInvoked bool
	mockAnswer := "table users has id, email, password_hash"

	handler := func(ctx context.Context, peerRole, question string) (string, error) {
		handlerInvoked = true
		if got, want := peerRole, "scout"; got != want {
			t.Errorf("%s(handler): got peerRole=%q, want %q", fn, got, want)
		}
		if got, want := question, "schema?"; got != want {
			t.Errorf("%s(handler): got question=%q, want %q", fn, got, want)
		}
		return mockAnswer, nil
	}

	tool := NewAskPeerTool(handler)
	rawArgs := []byte(`{
		"peer_role": "scout",
		"question": "schema?"
	}`)
	inputs := string(rawArgs)

	res, err := tool.Execute(&tools.ToolContext{Context: context.Background()}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%s): got unexpected error %v, want nil", fn, inputs, err)
	}

	if got, want := handlerInvoked, true; got != want {
		t.Fatalf("%s(inputs=%s): got handlerInvoked=%v, want %v", fn, inputs, got, want)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%s): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	expectedContent := fmt.Sprintf("[scout 的回答]: %s", mockAnswer)
	if got, want := res.Content, expectedContent; got != want {
		t.Fatalf("%s(inputs=%s): got content %q, want %q", fn, inputs, got, want)
	}
}

// TC-PEER-02: AskPeer必填参数缺失校验
func TestAskPeerTool_TC_PEER_02_RequiredParamsValidation(t *testing.T) {
	fn := "AskPeerTool.Execute"

	cases := []struct {
		name        string
		rawArgs     string
		wantSubstr  string
		shouldError bool
	}{
		{
			name:        "missing_peer_role",
			rawArgs:     `{"peer_role": "", "question": "schema?"}`,
			wantSubstr:  "peer_role cannot be empty",
			shouldError: true,
		},
		{
			name:        "missing_question",
			rawArgs:     `{"peer_role": "scout", "question": ""}`,
			wantSubstr:  "question cannot be empty",
			shouldError: true,
		},
		{
			name:        "missing_both",
			rawArgs:     `{"peer_role": "", "question": ""}`,
			wantSubstr:  "peer_role cannot be empty",
			shouldError: true,
		},
		{
			name:        "malformed_json",
			rawArgs:     `{malformed_json`,
			wantSubstr:  "invalid ask_peer_agent args",
			shouldError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var handlerInvoked bool
			tool := NewAskPeerTool(func(ctx context.Context, peerRole, question string) (string, error) {
				handlerInvoked = true
				return "unexpected answer", nil
			})

			res, err := tool.Execute(&tools.ToolContext{Context: context.Background()}, []byte(tc.rawArgs))
			if tc.shouldError {
				if err == nil {
					t.Fatalf("%s(case=%s, inputs=%s): got error nil, want error containing %q (res=%+v)",
						fn, tc.name, tc.rawArgs, tc.wantSubstr, res)
				}
				if !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Fatalf("%s(case=%s, inputs=%s): got error %q, want containing %q",
						fn, tc.name, tc.rawArgs, err.Error(), tc.wantSubstr)
				}
				if got, want := handlerInvoked, false; got != want {
					t.Fatalf("%s(case=%s, inputs=%s): got handlerInvoked=%v, want %v (handler must not be called)",
						fn, tc.name, tc.rawArgs, got, want)
				}
			}
		})
	}
}

func TestAskPeerTool_Contract(t *testing.T) {
	fn := "AskPeerTool.Contract"
	tool := NewAskPeerTool(nil)

	if got, want := tool.Name(), "ask_peer_agent"; got != want {
		t.Errorf("%s: got Name()=%q, want %q", fn, got, want)
	}
	if tool.Description() == "" {
		t.Errorf("%s: Description() is empty", fn)
	}
	if got, want := tool.ReplayMode(), tools.ReplaySafe; got != want {
		t.Errorf("%s: got ReplayMode()=%q, want %q", fn, got, want)
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("%s: InputSchema() is invalid json: %v", fn, err)
	}
}

func TestAskPeerTool_HandlerError(t *testing.T) {
	fn := "AskPeerTool.Execute"
	customErr := errors.New("peer agent is currently unreachable")
	tool := NewAskPeerTool(func(ctx context.Context, peerRole, question string) (string, error) {
		return "", customErr
	})

	rawArgs := []byte(`{
		"peer_role": "reviewer",
		"question": "Can you check this?"
	}`)
	inputs := string(rawArgs)

	_, err := tool.Execute(nil, rawArgs)
	if err == nil {
		t.Fatalf("%s(inputs=%s): got error nil, want error from handler", fn, inputs)
	}
	wantSubstr := "peer agent query failed"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("%s(inputs=%s): got error %q, want containing %q", fn, inputs, err.Error(), wantSubstr)
	}
}
