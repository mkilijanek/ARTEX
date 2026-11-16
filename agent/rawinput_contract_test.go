package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type rawContractProvider struct {
	captureUsageProvider
	input json.RawMessage
	calls int
}

func (p *rawContractProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if p.calls == 1 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "repair", Name: "Repair", Input: p.input}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
}

func TestDecoratedRawInputUsesSDKGateAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		raw, deny, bare bool
		input           string
		want            bool
	}{
		{"null", true, false, false, `null`, true},
		{"repair", true, false, false, `{"content":`, true},
		{"strict", false, false, false, `{"content":`, false},
		{"denied", true, true, false, `{"content":`, false},
		{"absent-interface", true, false, true, `{"content":`, false},
		{"defaults", false, false, false, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			base := tool.Build(tool.Spec{Name: "Repair", RawInput: tc.raw, Schema: map[string]any{"type": "object", "required": []any{"content"}}, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				if tc.deny {
					return permission.Denied("fixture")
				}
				return permission.Allowed()
			}, Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
				called = true
				if tc.raw && string(in) != tc.input {
					t.Errorf("raw input changed: %q", in)
				}
				if tc.name == "defaults" && string(in) != `{"content":"default"}` {
					t.Errorf("defaults lost: %s", in)
				}
				return tool.Text("repaired"), nil
			}})
			var wrapped tool.CoreTool = base
			if tc.bare {
				wrapped = struct{ tool.CoreTool }{base}
			}
			schema := map[string]any{"type": "object"}
			if tc.name == "defaults" || tc.name == "null" {
				schema["properties"] = map[string]any{"content": map[string]any{"type": "string", "default": "default"}}
			} else {
				schema["required"] = []any{"content"}
			}
			wrapped = guardPanic(DecorateTool(guardPanic(wrapped), "", schema))
			p := &rawContractProvider{input: json.RawMessage(tc.input)}
			_, _, err := captureRun(t.Context(), agentcore.Options{Provider: p, Tools: []tool.CoreTool{wrapped}, NonStreaming: true, MaxTurns: 3, PermissionMode: permission.ModeBypass}, "fixture", nil)
			if err != nil || called != tc.want {
				t.Fatalf("executed=%v want=%v err=%v", called, tc.want, err)
			}
		})
	}
}

func TestToolHookCanReplaceItself(t *testing.T) {
	old := ToolAugment()
	defer SetToolAugment(old)
	SetToolAugment(func(context.Context, string) ([]tool.CoreTool, DeferredInfo, func()) {
		SetToolAugment(nil)
		return nil, DeferredInfo{}, nil
	})
	_, _, cleanup := AugmentTools(t.Context(), "assistant", nil)
	cleanup()
	if ToolAugment() != nil {
		t.Fatal("reentrant replacement lost")
	}
}
