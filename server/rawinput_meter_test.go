package server

import (
	"context"
	"encoding/json"
	"github.com/mkilijanek/artex/agent"
	"iter"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type rawContractProvider struct {
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

func TestMeteredRawInputUsesSDKGateAndPermissions(t *testing.T) {
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
			wrapped = agent.DecorateTool(&meteredSkill{CoreTool: meterTool(agent.DecorateTool(wrapped, "", schema), &recordingToolUsage{}, "repair", "fixture", agent.RunInfo{})}, "", schema)
			p := &rawContractProvider{input: json.RawMessage(tc.input)}
			session := agentcore.NewSession(agentcore.Options{Provider: p, Tools: []tool.CoreTool{wrapped}, NonStreaming: true, MaxTurns: 3, PermissionMode: permission.ModeBypass})
			defer session.Close()
			var err error
			for _, e := range session.Prompt(t.Context(), "fixture") {
				if e != nil {
					err = e
				}
			}
			if err != nil || called != tc.want {
				t.Fatalf("executed=%v want=%v err=%v", called, tc.want, err)
			}
		})
	}
}

func (p *rawContractProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	panic("unexpected stream")
}
