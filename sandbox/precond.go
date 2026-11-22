package sandbox

import (
	"bytes"
	"debug/elf"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Probe is how the preconditions read the platform. The server reads it itself; no tool
// process is involved.
type Probe interface {
	EUID() int
	StatusFile() ([]byte, error) // /proc/self/status
	// Stat does not follow a final link. It returns the mode and the owner.
	Stat(path string) (fs.FileMode, uint32, error)
	IsStatic(path string) (bool, error)
	Getenv(key string) string
}

// Capability numbers (linux/capability.h) the drop needs.
const (
	capSetGID  = 6
	capSetUID  = 7
	capSetPCap = 8
)

// CheckPreconditions evaluates P1-P5.
func CheckPreconditions(l Layout, p Probe) Preconditions {
	pre := Preconditions{Reasons: map[string]string{}}
	fail := func(id, why string) { pre.Reasons[id] = why }

	// P1: the server is root.
	pre.P1 = p.EUID() == 0
	if !pre.P1 {
		fail("P1", fmt.Sprintf("the server does not run as root (uid %d)", p.EUID()))
	}

	// P2: it holds the capabilities the drop calls need.
	pre.P2 = true
	if st, err := p.StatusFile(); err != nil {
		pre.P2 = false
		fail("P2", "cannot read the capability sets: "+err.Error())
	} else if eff, err := parseCapEff(st); err != nil {
		pre.P2 = false
		fail("P2", err.Error())
	} else {
		var missing []string
		for _, c := range []struct {
			bit  uint
			name string
		}{{capSetUID, "CAP_SETUID"}, {capSetGID, "CAP_SETGID"}, {capSetPCap, "CAP_SETPCAP"}} {
			if eff&(1<<c.bit) == 0 {
				missing = append(missing, c.name)
			}
		}
		if len(missing) > 0 {
			pre.P2 = false
			fail("P2", strings.Join(missing, ", ")+" missing from the effective set")
		}
	}

	// P3: setpriv is a root-owned executable regular file nobody else can replace.
	if why := trustedFile(p, l.Setpriv, true); why != "" {
		fail("P3", why)
	} else {
		pre.P3 = true
	}

	// P4: the binary the root stage runs is static, and the shim directory is root's. The
	// binary's own owner is not part of the check: the permission step makes it root's before the
	// first tool process starts.
	pre.P4 = true
	if mode, _, err := p.Stat(l.Artex); err != nil {
		pre.P4 = false
		fail("P4", l.Artex+": "+err.Error())
	} else if !mode.IsRegular() || mode.Perm()&0o100 == 0 {
		pre.P4 = false
		fail("P4", l.Artex+" is not an executable regular file")
	} else if static, err := p.IsStatic(l.Artex); err != nil {
		pre.P4 = false
		fail("P4", l.Artex+": "+err.Error())
	} else if !static {
		pre.P4 = false
		fail("P4", l.Artex+" is dynamically linked")
	}
	if pre.P4 {
		if mode, uid, err := p.Stat(l.ShimDir); err != nil {
			pre.P4 = false
			fail("P4", "the shim directory is missing: "+err.Error())
		} else if !mode.IsDir() || uid != 0 || mode.Perm()&0o022 != 0 {
			pre.P4 = false
			fail("P4", "the shim directory is not a root-owned directory")
		} else if why := trustedFile(p, l.ShimDir+"/bash", true); why != "" {
			pre.P4 = false
			fail("P4", why)
		}
	}

	// P5: the operator did not turn it off. An empty value means unset; only "off" disables.
	pre.P5 = strings.ToLower(strings.TrimSpace(p.Getenv(EnvIsolation))) != "off"
	if !pre.P5 {
		fail("P5", EnvIsolation+"=off")
	}
	return pre
}

// trustedFile returns why path is not a root-owned regular file that only root can write, or
// "" when it is. exec additionally requires an execute bit.
func trustedFile(p Probe, path string, exec bool) string {
	mode, uid, err := p.Stat(path)
	if err != nil {
		return path + ": " + err.Error()
	}
	switch {
	case !mode.IsRegular():
		return path + " is not a regular file"
	case uid != 0:
		return path + " is not owned by root"
	case mode.Perm()&0o022 != 0:
		return path + " is writable by group or others"
	case exec && mode.Perm()&0o100 == 0:
		return path + " is not executable"
	}
	return ""
}

// parseCapEff reads the CapEff line of /proc/<pid>/status.
func parseCapEff(status []byte) (uint64, error) {
	for _, line := range bytes.Split(status, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("CapEff:")); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(string(v)), 16, 64)
			if err != nil {
				return 0, fmt.Errorf("unreadable CapEff line: %w", err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("no CapEff line in the process status")
}

// HostProbe reads the real platform.
type HostProbe struct{ Layout Layout }

func (HostProbe) EUID() int { return os.Geteuid() }
func (h HostProbe) StatusFile() ([]byte, error) {
	return os.ReadFile(h.Layout.ProcRoot + "/self/status")
}
func (HostProbe) Stat(path string) (fs.FileMode, uint32, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	uid, ok := fileUID(fi)
	if !ok {
		return 0, 0, fmt.Errorf("%s: the owner is not reported on this platform", path)
	}
	return fi.Mode(), uid, nil
}
func (HostProbe) Getenv(key string) string { return os.Getenv(key) }

// IsStatic reports whether the ELF file has no interpreter and no needed library, so the
// dynamic loader never runs for it.
func (HostProbe) IsStatic(path string) (bool, error) {
	f, err := elf.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false, nil
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return false, err
	}
	return len(libs) == 0, nil
}
