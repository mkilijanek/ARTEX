package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestChatSessionIdentityWithoutTranscript(t *testing.T) {
	var ids []string
	p := captureUsageProvider{stream: func(ctx context.Context, yield func(llm.StreamEvent, error) bool) {
		ids = append(ids, transcript.SessionIDFrom(ctx))
		if yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		}
	}}
	c := NewChatAgent(p, "fixture", t.TempDir(), nil, 100000)
	for range 2 {
		if _, err := c.Chat(t.Context(), "assistant", "conv-stable", "hello", 1, 0, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 2 || ids[0] != "conv-stable" || ids[1] != ids[0] {
		t.Fatalf("session identities: %v", ids)
	}
}

func TestChatRefusesCorruptOrUnreadableTranscript(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrupt", true: "unreadable-directory"}[unreadable], func(t *testing.T) {
			dir := t.TempDir()
			store := transcript.NewStore(dir)
			path := store.MainPath("conv-broken")
			const original = "{corrupt transcript\n"
			if unreadable {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := captureUsageProvider{stream: func(context.Context, func(llm.StreamEvent, error) bool) { calls++ }}
			chat := NewChatAgent(p, "fixture", filepath.Join(dir, "work"), store, 100000)
			_, err := chat.Chat(t.Context(), "assistant", "conv-broken", "hello", 1, 0, false, nil)
			if err == nil || !strings.Contains(err.Error(), "restore Norma session") || calls != 0 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if !unreadable {
				b, e := os.ReadFile(path)
				if e != nil || string(b) != original {
					t.Fatal("damaged transcript changed", e)
				}
			}
		})
	}
}

func TestResumeMissingThenSignedHistory(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	s := agentcore.NewSession(agentcore.Options{Transcript: store, SessionID: "stable"})
	defer s.Close()
	if err := resumeSession(s, "stable"); err != nil {
		t.Fatal(err)
	}
	w := store.NewWriter("stable", "")
	w.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "", Signature: "signed"}, {Type: llm.BlockRedactedThinking, Data: "opaque"}}}, llm.Usage{})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	if err := resumeSession(s, "stable"); err != nil {
		t.Fatal(err)
	}
	m := s.Messages()
	if len(m) != 1 || len(m[0].Content) != 2 || m[0].Content[0].Signature != "signed" || m[0].Content[1].Data != "opaque" {
		t.Fatalf("lost signed history: %+v", m)
	}
}
