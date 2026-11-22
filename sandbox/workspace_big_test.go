//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ARTEX_WORKSPACE_BIG=<entries> builds a root-owned tree of about that many entries and walks it
// with the shipped bounds until it is complete (at most four passes), then once more, logging what each walk did. It is a measurement, not a
// gate: nothing is asserted about the time.
func TestMigrateLargeTreeMeasurement(t *testing.T) {
	needRoot(t)
	v := os.Getenv("ARTEX_WORKSPACE_BIG")
	if v == "" {
		t.Skip("set ARTEX_WORKSPACE_BIG=<entries> to measure")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1000 {
		t.Fatalf("ARTEX_WORKSPACE_BIG=%q", v)
	}
	ws := filepath.Join(t.TempDir(), "w")
	dirs := n / 200
	built := time.Now()
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(ws, fmt.Sprintf("d%05d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 199; f++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", f)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("built %d entries (root-owned) in %s", dirs*200, time.Since(built).Round(time.Millisecond))
	lb := &logBuf{}
	for pass := 1; pass <= 4; pass++ {
		o := DefaultMigrateOptions()
		o.Logf = lb.logf
		rep, err := MigrateWorkspace(t.Context(), ws, o)
		t.Logf("pass %d: %+v err=%v", pass, rep, err)
		if rep.Complete {
			break
		}
	}
	o := DefaultMigrateOptions()
	o.Logf = lb.logf
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	t.Logf("finished tree, shipped bounds: %+v err=%v", rep, err)
	t.Logf("log:\n%s", lb.text())
}
