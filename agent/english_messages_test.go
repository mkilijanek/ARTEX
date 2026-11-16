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
		Patched  map[string]string `json:"patched_sha256"`
		Extra    map[string]string `json:"extra_sha256"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for rel, hash := range manifest.Original {
		expected[rel] = hash
	}
	for rel, hash := range manifest.Patched {
		if _, ok := manifest.Original[rel]; !ok {
			t.Fatalf("patch lacks upstream file: %s", rel)
		}
		expected[rel] = hash
	}
	for rel, hash := range manifest.Extra {
		if _, ok := expected[rel]; ok {
			t.Fatalf("extra overlaps upstream: %s", rel)
		}
		expected[rel] = hash
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "SOURCE.json" && rel != "README.artex.md" {
			if _, ok := expected[rel]; !ok {
				t.Errorf("unmanifested SDK file: %s", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range expected {
		path := filepath.Join(root, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("unrelated SDK source changed: %s", rel)
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
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
