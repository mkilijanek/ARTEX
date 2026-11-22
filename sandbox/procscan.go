package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// CountToolProcesses counts the processes of uid under procRoot that are not zombies. A
// zombie cannot act, so it is not a leftover. A process that exits while the directory is
// being read is skipped.
func CountToolProcesses(procRoot string, uid uint32) (int, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("list processes: %w", err)
	}
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil || !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "status"))
		if err != nil {
			if os.IsNotExist(err) {
				continue // gone since the listing
			}
			// A process that exited between the open and the read reports ESRCH.
			if pe, ok := err.(*os.PathError); ok && isNoProcess(pe.Err) {
				continue
			}
			return 0, fmt.Errorf("read process %s: %w", e.Name(), err)
		}
		state, realUID, ok := parseStatus(data)
		if !ok || realUID != uid {
			continue
		}
		if state == 'Z' || state == 'X' {
			continue
		}
		n++
	}
	return n, nil
}

// parseStatus extracts the state letter and the real uid from /proc/<pid>/status.
func parseStatus(data []byte) (state byte, uid uint32, ok bool) {
	var haveState, haveUID bool
	for _, line := range bytes.Split(data, []byte("\n")) {
		if v, found := bytes.CutPrefix(line, []byte("State:")); found {
			v = bytes.TrimSpace(v)
			if len(v) > 0 {
				state, haveState = v[0], true
			}
		} else if v, found := bytes.CutPrefix(line, []byte("Uid:")); found {
			f := bytes.Fields(v)
			if len(f) > 0 {
				if n, err := strconv.ParseUint(string(f[0]), 10, 32); err == nil {
					uid, haveUID = uint32(n), true
				}
			}
		}
	}
	return state, uid, haveState && haveUID
}
