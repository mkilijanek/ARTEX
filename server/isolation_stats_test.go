//go:build unix

package server

import (
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mkilijanek/artex/sandbox"
)

// platform is a host that cannot drop (the server is not root), for the unavailable status.
type platform struct{ l sandbox.Layout }

func (platform) EUID() int                                { return 1000 }
func (platform) StatusFile() ([]byte, error)              { return []byte("CapEff:\t0000000000000000\n"), nil }
func (platform) Stat(string) (fs.FileMode, uint32, error) { return 0o755, 0, nil }
func (platform) IsStatic(string) (bool, error)            { return true, nil }
func (platform) Getenv(string) string                     { return "" }

func activateUnavailable(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	l := sandbox.DefaultLayout()
	l.Artex, l.ShimDir, l.ProcRoot = filepath.Join(root, "app", "artex"), filepath.Join(root, "shims"), filepath.Join(root, "proc")
	l.PrivateDir, l.VarsDir, l.ServerTmp = filepath.Join(root, "p"), filepath.Join(root, "p", "v"), filepath.Join(root, "t")
	data := filepath.Join(root, "app", "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	iso, err := sandbox.Activate(t.Context(), sandbox.Config{Layout: l, DataDir: data, KeyDir: filepath.Dir(l.Artex), Probe: platform{l}, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	sandbox.Set(iso)
	t.Cleanup(func() { sandbox.Set(nil) })
}

// AC22: /api/stats reports the isolation status, the reason and the warnings; /api/health, which
// anyone can call, says nothing about it.
func TestStatsReportsToolIsolationAndHealthDoesNot(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", testDSN())
	if err != nil {
		t.Skipf("no manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	s := &Server{m: m, engine: NewEngine(m)}
	activateUnavailable(t)

	w := httptest.NewRecorder()
	s.stats(w, httptest.NewRequest("GET", "/api/stats", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		ToolIsolation struct {
			Status   string   `json:"status"`
			Reason   string   `json:"reason"`
			Warnings []string `json:"warnings"`
		} `json:"tool_isolation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ToolIsolation.Status != "unavailable" || !strings.Contains(out.ToolIsolation.Reason, "P1") {
		t.Fatalf("tool_isolation %+v", out.ToolIsolation)
	}
	if !strings.Contains(w.Body.String(), `"warnings":[]`) {
		t.Errorf("warnings must be an empty list, not null: %s", w.Body.String())
	}

	hw := httptest.NewRecorder()
	s.health(hw, httptest.NewRequest("GET", "/api/health", nil))
	if hw.Code != 200 || strings.Contains(strings.ToLower(hw.Body.String()), "isolation") || strings.Contains(hw.Body.String(), "unavailable") {
		t.Fatalf("health: %d %s", hw.Code, hw.Body.String())
	}
}

func ownerUID(t *testing.T, path string) uint32 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Uid
}
