package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestSessionIdentityAcrossRoleEntryPoints(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("requires isolated PostgreSQL")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("only local session fixtures", "SDK identity")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	for _, persist := range []bool{false, true} {
		t.Run(fmt.Sprintf("transcript=%v", persist), func(t *testing.T) {
			dir := t.TempDir()
			var store *transcript.Store
			if persist {
				store = transcript.NewStore(filepath.Join(dir, "transcripts"))
			}
			for _, role := range []string{"worker", "planner", "main-zero", "main-segment"} {
				t.Run(role, func(t *testing.T) {
					var ids []string
					provider := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
						ids = append(ids, transcript.SessionIDFrom(ctx))
						if y(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
							y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
						}
					}}
					want := ""
					run := func() error { return nil }
					switch role {
					case "worker":
						w := NewWorker(provider, "fixture", dir, store, 100000, 1)
						want = WorkerSessionID(exp, 101)
						run = func() error {
							_, _, err := w.Execute(t.Context(), "fixture", exp, nil, ts, &db.Node{ID: 101, Payload: json.RawMessage(`{"summary":"local session fixture"}`)}, nil, nil, nil, nil)
							return err
						}
					case "planner":
						p := NewPlanner(provider, "fixture", dir, store, 100000, 1)
						want = fmt.Sprintf("exp%d-planner", exp)
						run = func() error {
							_, _, err := p.Plan(t.Context(), exp, nil, ts, "local session fixture", nil, nil)
							return err
						}
					default:
						m := NewMainAgent(provider, "fixture", dir, store, 100000, 1)
						segment := 0
						want = fmt.Sprintf("exp%d-main", exp)
						if role == "main-segment" {
							segment = 2
							want += "-s2"
						}
						run = func() error {
							_, err := m.Chat(t.Context(), exp, segment, nil, ts, "local session fixture", "message", nil, nil, nil, nil, nil)
							return err
						}
					}
					for range 2 {
						if err := run(); err != nil {
							t.Fatal(err)
						}
					}
					if len(ids) != 2 || ids[0] != want || ids[1] != want {
						t.Fatalf("SDK provider identities=%v want %s", ids, want)
					}
					if persist {
						if _, err := os.Stat(store.MainPath(want)); err != nil {
							t.Fatal("missing role transcript", err)
						}
					}
				})
			}
			files, _ := filepath.Glob(filepath.Join(dir, "transcripts", "*.jsonl"))
			if !persist && len(files) != 0 {
				t.Fatal("store-free runs wrote transcripts")
			}
			if persist && len(files) != 4 {
				t.Fatalf("segment/role files=%v", files)
			}
		})
	}
}

func TestResumedChatUnlocksToolsBeforeCallingModel(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	w := store.NewWriter("conv-unlock", "")
	for _, m := range []llm.Message{llm.UserText("initial"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "skill-call", Name: "Skill", Input: json.RawMessage(`{"name":"fixture"}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "skill-call", Content: []llm.ContentBlock{llm.TextBlock("loaded")}}}}} {
		w.RecordMessage(m, llm.Usage{})
	}
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	old := ToolAugment()
	defer SetToolAugment(old)
	unlocked := false
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return nil, DeferredInfo{UnlockSkill: func(name string) {
			if name == "fixture" {
				unlocked = true
			}
		}}, nil
	})
	provider := captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
		if !unlocked {
			t.Error("model called before skill replay")
		}
		if y(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		}
	}}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), store, 100000)
	if _, err := chat.Chat(t.Context(), "assistant", "conv-unlock", "continue", 1, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if !unlocked {
		t.Fatal("successful restore lost deferred unlock state")
	}
	before, err := os.ReadFile(store.MainPath("conv-unlock"))
	if err != nil || !strings.Contains(string(before), "skill-call") {
		t.Fatal("restored tool history lost", err)
	}
}

func TestCorruptHistoryAcrossRoleEntryPoints(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("requires isolated PostgreSQL")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("only local session fixtures", "SDK identity")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	for _, persist := range []bool{true} {
		t.Run(fmt.Sprintf("transcript=%v", persist), func(t *testing.T) {
			dir := t.TempDir()
			var store *transcript.Store
			if persist {
				store = transcript.NewStore(filepath.Join(dir, "transcripts"))
			}
			for _, role := range []string{"worker", "main-zero", "main-segment"} {
				t.Run(role, func(t *testing.T) {
					var ids []string
					provider := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
						ids = append(ids, transcript.SessionIDFrom(ctx))
						if y(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
							y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
						}
					}}
					want := ""
					run := func() error { return nil }
					switch role {
					case "worker":
						w := NewWorker(provider, "fixture", dir, store, 100000, 1)
						want = WorkerSessionID(exp, 101)
						run = func() error {
							_, _, err := w.Execute(t.Context(), "fixture", exp, nil, ts, &db.Node{ID: 101, Payload: json.RawMessage(`{"summary":"local session fixture"}`)}, nil, nil, nil, nil)
							return err
						}
					case "planner":
						p := NewPlanner(provider, "fixture", dir, store, 100000, 1)
						want = fmt.Sprintf("exp%d-planner", exp)
						run = func() error {
							_, _, err := p.Plan(t.Context(), exp, nil, ts, "local session fixture", nil, nil)
							return err
						}
					default:
						m := NewMainAgent(provider, "fixture", dir, store, 100000, 1)
						segment := 0
						want = fmt.Sprintf("exp%d-main", exp)
						if role == "main-segment" {
							segment = 2
							want += "-s2"
						}
						run = func() error {
							_, err := m.Chat(t.Context(), exp, segment, nil, ts, "local session fixture", "message", nil, nil, nil, nil, nil)
							return err
						}
					}
					const broken = "{torn transcript\n"
					path := store.MainPath(want)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
						t.Fatal(err)
					}
					err := run()
					if err == nil || !strings.Contains(err.Error(), path) || len(ids) != 0 {
						t.Fatalf("err=%v model calls=%v", err, ids)
					}
					body, err := os.ReadFile(path)
					if err != nil || string(body) != broken {
						t.Fatalf("damaged history changed: %q %v", body, err)
					}

				})
			}
			files, _ := filepath.Glob(filepath.Join(dir, "transcripts", "*.jsonl"))
			if !persist && len(files) != 0 {
				t.Fatal("store-free runs wrote transcripts")
			}
			if persist && len(files) != 3 {
				t.Fatalf("segment/role files=%v", files)
			}
		})
	}
}
