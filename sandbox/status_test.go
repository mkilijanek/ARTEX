package sandbox

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// fakeProbe is a platform that satisfies P1-P5 until a field says otherwise.
type fakeProbe struct {
	l         *Layout // paths the probe answers for; the default layout when nil
	euid      int
	capEff    string // hex, as /proc/self/status prints it
	setpriv   *fakeFile
	static    bool
	staticErr error
	shimDir   *fakeFile
	shim      *fakeFile
	env       string
}

type fakeFile struct {
	mode fs.FileMode
	uid  uint32
}

func goodProbe() *fakeProbe {
	return &fakeProbe{
		euid:    0,
		capEff:  "00000000a80425fb", // CAP_SETGID, CAP_SETUID and CAP_SETPCAP are in Docker's default set
		setpriv: &fakeFile{mode: 0o755, uid: 0},
		static:  true,
		shimDir: &fakeFile{mode: fs.ModeDir | 0o755, uid: 0},
		shim:    &fakeFile{mode: 0o755, uid: 0},
	}
}

func (p *fakeProbe) EUID() int { return p.euid }
func (p *fakeProbe) StatusFile() ([]byte, error) {
	return []byte("Name:\tartex\nCapInh:\t0000000000000000\nCapPrm:\t" + p.capEff + "\nCapEff:\t" + p.capEff + "\nCapBnd:\t" + p.capEff + "\n"), nil
}
func (p *fakeProbe) layout() Layout {
	if p.l != nil {
		return *p.l
	}
	return DefaultLayout()
}
func (p *fakeProbe) Stat(path string) (fs.FileMode, uint32, error) {
	var f *fakeFile
	l := p.layout()
	switch path {
	case l.Setpriv:
		f = p.setpriv
	case l.ShimDir:
		f = p.shimDir
	case l.ShimDir + "/bash":
		f = p.shim
	case l.Artex:
		f = &fakeFile{mode: 0o755}
	}
	if f == nil {
		return 0, 0, fs.ErrNotExist
	}
	return f.mode, f.uid, nil
}
func (p *fakeProbe) IsStatic(string) (bool, error) { return p.static, p.staticErr }
func (p *fakeProbe) Getenv(string) string          { return p.env }

func TestPreconditionsHoldOnTheShippedPlatform(t *testing.T) {
	pre := CheckPreconditions(DefaultLayout(), goodProbe())
	if len(pre.Failing()) != 0 {
		t.Fatalf("failing: %v", pre.Failing())
	}
}

// Each precondition has its own way to fail, and each is reported under its own name.
func TestPreconditionsEachFailsOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		id  string
		mut func(*fakeProbe)
	}{
		{"P1", func(p *fakeProbe) { p.euid = 1000 }},
		{"P2", func(p *fakeProbe) { p.capEff = "00000000a80425fb" }}, // replaced below
		{"P3", func(p *fakeProbe) { p.setpriv = nil }},
		{"P3", func(p *fakeProbe) { p.setpriv.uid = 1000 }},
		{"P3", func(p *fakeProbe) { p.setpriv.mode = 0o644 }},
		{"P3", func(p *fakeProbe) { p.setpriv.mode = fs.ModeSymlink | 0o755 }},
		{"P4", func(p *fakeProbe) { p.static = false }},
		{"P4", func(p *fakeProbe) {
			p.l = &Layout{Artex: "/nonexistent/artex", ShimDir: DefaultLayout().ShimDir, Setpriv: DefaultLayout().Setpriv}
		}},
		{"P4", func(p *fakeProbe) { p.staticErr = fs.ErrNotExist }},
		{"P4", func(p *fakeProbe) { p.shimDir = nil }},
		{"P4", func(p *fakeProbe) { p.shimDir.uid = 1000 }},
		{"P4", func(p *fakeProbe) { p.shim = nil }},
		{"P4", func(p *fakeProbe) { p.shim.uid = 1000 }},
		{"P5", func(p *fakeProbe) { p.env = "off" }},
		{"P5", func(p *fakeProbe) { p.env = " OFF " }},
	} {
		p := goodProbe()
		if tc.id == "P2" {
			// every one of the three capabilities, removed one at a time
			for _, bit := range []uint{6, 7, 8} { // CAP_SETGID, CAP_SETUID, CAP_SETPCAP
				q := goodProbe()
				q.capEff = fmt.Sprintf("%016x", uint64(0xa80425fb)&^(1<<bit))
				if got := CheckPreconditions(DefaultLayout(), q).Failing(); len(got) != 1 || got[0] != "P2" {
					t.Errorf("capability bit %d removed: failing %v, want [P2]", bit, got)
				}
			}
			q := goodProbe()
			q.capEff = "0000000000000000"
			if got := CheckPreconditions(DefaultLayout(), q).Failing(); len(got) != 1 || got[0] != "P2" {
				t.Errorf("no capabilities: failing %v, want [P2]", got)
			}
			continue
		}
		tc.mut(p)
		got := CheckPreconditions(DefaultLayout(), p).Failing()
		if len(got) != 1 || got[0] != tc.id {
			t.Errorf("%s case: failing %v, want [%s]", tc.id, got, tc.id)
		}
	}
}

// The override is read from the environment only; an empty value means unset, and an
// unrecognised value never switches isolation off.
func TestIsolationOverrideValues(t *testing.T) {
	for v, wantOff := range map[string]bool{"": false, "  ": false, "off": true, "OFF": true, "on": false, "1": false, "no": false, "false": false} {
		p := goodProbe()
		p.env = v
		off := !CheckPreconditions(DefaultLayout(), p).P5
		if off != wantOff {
			t.Errorf("ARTEX_TOOL_ISOLATION=%q: off=%v, want %v", v, off, wantOff)
		}
	}
}

// AC22(g): `unavailable` appears exactly when P1-P4 fail, `disabled` exactly when the
// operator turned it off, and no combination of C1-C11 failures ever changes that. A
// failure a tool can cause (every C check) leaves isolation on.
func TestStatusFollowsPreconditionsOnly(t *testing.T) {
	checkIDs := []string{"C1", "C2", "C3", "C4", "C5", "C6", "C7", "C8", "C9", "C10", "C11"}
	degrading := map[string]bool{"C1": true, "C2": true, "C3": true, "C4": true, "C5": true, "C6": true, "C10": true, "C11": true}
	for p := 0; p < 32; p++ {
		pre := Preconditions{P1: p&1 != 0, P2: p&2 != 0, P3: p&4 != 0, P4: p&8 != 0, P5: p&16 != 0}
		pHold := pre.P1 && pre.P2 && pre.P3 && pre.P4
		for c := 0; c < 1<<len(checkIDs); c++ {
			var checks []CheckResult
			anyDegrading := false
			for i, id := range checkIDs {
				if c&(1<<i) != 0 {
					checks = append(checks, CheckResult{ID: id, Detail: "failed", Degrades: degrading[id]})
					anyDegrading = anyDegrading || degrading[id]
				}
			}
			st, _ := ComputeStatus(pre, checks)
			var want Status
			switch {
			case !pre.P5:
				want = Disabled
			case !pHold:
				want = Unavailable
			case anyDegrading:
				want = Degraded
			default:
				want = Enforced
			}
			if st != want {
				t.Fatalf("P=%v checks=%v: status %s, want %s", pre, checks, st, want)
			}
			if st == Unavailable && pHold {
				t.Fatalf("unavailable although P1-P4 hold (checks %v)", checks)
			}
			if got := st.IsolationOn(); got != (st == Enforced || st == Degraded) {
				t.Fatalf("%s: IsolationOn=%v", st, got)
			}
		}
	}
}

func TestComputeStatusReasonsAndWarnings(t *testing.T) {
	pre := Preconditions{P1: true, P2: true, P3: true, P4: true, P5: true}
	st, view := ComputeStatus(pre, []CheckResult{
		{ID: "C3", Detail: "the config file is readable", Degrades: true},
		{ID: "C7", Detail: "database TCP is reachable"},
	})
	if st != Degraded {
		t.Fatalf("status %s", st)
	}
	if !strings.Contains(view.Reason, "C3") || strings.Contains(view.Reason, "C7") {
		t.Errorf("reason %q should name the degrading check only", view.Reason)
	}
	if len(view.Warnings) != 2 {
		t.Errorf("warnings %v, want both checks", view.Warnings)
	}
	// warnings alone keep the status enforced
	st, view = ComputeStatus(pre, []CheckResult{{ID: "C9", Detail: "metadata reachable"}})
	if st != Enforced || len(view.Warnings) != 1 || view.Reason != "" {
		t.Errorf("status %s view %+v", st, view)
	}
	// the reason of an unavailable status names the failing precondition
	st, view = ComputeStatus(Preconditions{P1: true, P2: false, P3: true, P4: true, P5: true, Reasons: map[string]string{"P2": "CAP_SETUID is missing"}}, nil)
	if st != Unavailable || !strings.Contains(view.Reason, "CAP_SETUID") {
		t.Errorf("status %s view %+v", st, view)
	}
	if view.Warnings == nil {
		t.Error("Warnings must be an empty list, not nil, so the API never returns null")
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
