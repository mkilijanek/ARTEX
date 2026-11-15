package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/Autumn-27/norma/noa"
	actool "github.com/Autumn-27/norma/tool"
)

func hasChinese(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || r == 0x3002 {
			return true
		}
	}
	return false
}

func TestSDKSystemMessageLiteralsAreEnglish(t *testing.T) {
	root := filepath.Join("..", "third_party", "norma")
	raw, err := os.ReadFile(filepath.Join(root, "SOURCE.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Original map[string]string `json:"original_sha256"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"noa/archive.go": true, "tool/task.go": true, "tool/shell_session.go": true, "tool/shell_tools.go": true}
	// Separately reviewed integration patches, documented in README.artex.md.
	allowed["noaadapter/compactor.go"] = true
	allowed["noaadapter/project.go"] = true
	allowed["noaadapter/session.go"] = true
	allowed["skill/skill.go"] = true // quote caller args inside privileged skill Extra
	allowed["noaadapter/materialize.go"] = true // retain host archive pointers with NOA off
	for rel, want := range manifest.Original {
		path := filepath.Join(root, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want && !allowed[rel] {
			t.Errorf("unrelated SDK source changed: %s", rel)
		}
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(tree, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, err := strconv.Unquote(lit.Value)
				if err == nil && hasChinese(value) {
					t.Errorf("untranslated system literal in %s", rel)
				}
			}
			return true
		})
	}
}

func TestShellMetadataAndValidationMessagesAreEnglish(t *testing.T) {
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "0")
	for _, tool := range actool.ShellSessionTools() {
		schema, _ := json.Marshal(tool.InputSchema())
		if hasChinese(tool.Description() + string(schema)) {
			t.Errorf("Chinese tool metadata: %s", tool.Name())
		}
		res, err := tool.Call(t.Context(), json.RawMessage(`{}`), nil)
		if err != nil || !res.IsError {
			t.Fatalf("expected missing-manager error for %s: %v", tool.Name(), err)
		}
		for _, block := range res.Content {
			if !strings.Contains(block.Text, "requires an enabled background task manager") {
				t.Errorf("unexpected message: %q", block.Text)
			}
		}
	}
}

func TestEnglishNoticesPreserveOriginalCommandOutput(t *testing.T) {
	t.Setenv("AGENT_CORE_DISABLE_BACKGROUND_TASKS", "0")
	mgr, err := actool.NewManager("english-message-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup()
	const evidence = "原始证据"
	task, err := mgr.Spawn(actool.SpawnSpec{Command: "printf %s " + evidence, Kind: actool.KindBash})
	if err != nil {
		t.Fatal(err)
	}
	mgr.Background(task.ID)
	if _, done := mgr.Wait(t.Context(), task.ID, 5*time.Second); !done {
		t.Fatal("fixture did not finish")
	}
	output, err := mgr.Output(task.ID, 4096)
	if err != nil || output != evidence {
		t.Fatalf("raw output changed: %q %v", output, err)
	}
	notices := mgr.DrainNotifications()
	if len(notices) != 1 || !strings.Contains(notices[0].Summary, "completed (exit 0). Use TaskOutput or Read") {
		t.Fatalf("completion message: %+v", notices)
	}
	archive := string(noa.RenderArchive(noa.ArchiveRenderInput{BlockID: "fixture", Tier: noa.Tier(1), Summary: evidence}))
	if !strings.Contains(archive, "# Archive fixture") || !strings.Contains(archive, "**Summary**") || !strings.Contains(archive, evidence) {
		t.Fatal("archive labels or original evidence changed")
	}
}
