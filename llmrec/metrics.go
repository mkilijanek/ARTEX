package llmrec

import (
	"context"
	"encoding/json"
	"github.com/Autumn-27/artex/llmmeta"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type observerKey struct{}
type attemptKey struct{}

// WithUsageObserver observes each provider invocation synchronously, once it
// finishes. Callers sharing an observer across goroutines must synchronize it.
// The value contains counters and runtime identifiers, never request content.
func WithUsageObserver(ctx context.Context, observe func(db.LLMUsage)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

// WithAttempt labels a physical provider invocation within one logical request.
// The task runtime increments it across both retries and profile failover.
func WithAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, attemptKey{}, max(0, attempt))
}

// ForProvider sets the adapter label during construction, before the Recorder
// is shared. It must not receive an endpoint URL or API key.
func (r *Recorder) ForProvider(provider string) *Recorder {
	r.provider = provider
	return r
}

func intentFromSession(session string) int64 {
	parts := strings.Split(session, "-")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "exp") || parts[1] != "worker" || !strings.HasPrefix(parts[2], "i") {
		return 0
	}
	return db.ParseExpID(strings.TrimPrefix(parts[2], "i"))
}

// measureRequest measures the provider-neutral representation. These numbers
// are neither provider wire sizes nor token estimates. ToolResultBytes is a
// subset of MessageBytes; the byte columns must not be summed as disjoint costs.
func measureRequest(req llm.CompletionRequest) db.LLMUsage {
	u := db.LLMUsage{SizeKnown: true, ToolBytes: map[string]db.ToolBytes{}}
	size := func(v any) int {
		b, err := json.Marshal(v)
		if err != nil {
			u.SizeKnown = false
			return 0
		}
		return len(b)
	}
	u.RequestBytes = size(req)
	for _, segment := range req.System {
		u.SystemBytes += len(segment)
	}
	if len(req.Messages) > 0 {
		u.MessageBytes = size(req.Messages)
	}
	toolNames := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				toolNames[b.ID] = b.Name
			}
		}
	}
	known := map[string]bool{}
	for _, tool := range req.Tools {
		known[tool.Name] = true
	}
	key := func(name string) string {
		if known[name] && len(name) <= 128 && !strings.ContainsAny(name, "\r\n\t") {
			return name
		}
		return "_unknown"
	}
	if len(req.Tools) > 0 {
		u.ToolSchemaBytes = size(req.Tools)
		for _, tool := range req.Tools {
			k := key(tool.Name)
			b := u.ToolBytes[k]
			b.Schema += size(tool)
			u.ToolBytes[k] = b
		}
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == llm.BlockToolResult {
				n := size(block)
				u.ToolResultBytes += n
				k := key(toolNames[block.ToolUseID])
				b := u.ToolBytes[k]
				b.Result += n
				u.ToolBytes[k] = b
			}
		}
	}
	return u
}

func invocationContext(ctx context.Context) context.Context {
	if n, ok := llmmeta.From(ctx).NextAttempt(); ok {
		return WithAttempt(ctx, n)
	}
	return ctx
}
