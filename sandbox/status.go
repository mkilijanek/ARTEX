package sandbox

import (
	"fmt"
	"strings"
)

// Status is where tool isolation stands. Sessions run in every status (warn, do not refuse).
type Status string

const (
	// Enforced: tool processes run as the tool user. The shipped image with default settings.
	Enforced Status = "enforced"
	// Degraded: the drop works but a secret, the skills directory or the database socket is
	// still reachable by tools, a check that runs a tool process failed, or the host's link
	// protection is off.
	Degraded Status = "degraded"
	// Unavailable: the drop cannot work on this platform (P1-P4). Tools run as the server's
	// user, as before.
	Unavailable Status = "unavailable"
	// Disabled: the operator set ARTEX_TOOL_ISOLATION=off. Same behaviour as Unavailable.
	Disabled Status = "disabled"
)

// IsolationOn reports whether tool processes go through the drop chain.
func (s Status) IsolationOn() bool { return s == Enforced || s == Degraded }

// Preconditions are the static facts that decide whether the drop can work. Only these can
// switch isolation off: a check that runs a tool process (and so could be made to fail by a
// tool) never does.
type Preconditions struct {
	P1, P2, P3, P4, P5 bool
	Reasons            map[string]string // why a precondition failed, by id
}

// Failing lists the failed preconditions in order.
func (p Preconditions) Failing() []string {
	var out []string
	for i, ok := range []bool{p.P1, p.P2, p.P3, p.P4, p.P5} {
		if !ok {
			out = append(out, fmt.Sprintf("P%d", i+1))
		}
	}
	return out
}

// DropPossible reports whether P1-P4 hold.
func (p Preconditions) DropPossible() bool { return p.P1 && p.P2 && p.P3 && p.P4 }

// CheckResult is one failed self-check. Passing checks are not recorded.
type CheckResult struct {
	ID       string
	Detail   string
	Degrades bool // C1-C6, C10 and C11 degrade the status; C7-C9 only warn unless unverifiable
}

// View is what the API and the banner show.
type View struct {
	Status   Status   `json:"status"`
	Reason   string   `json:"reason"`
	Warnings []string `json:"warnings"`
}

// ComputeStatus derives the status from the preconditions and the failed checks. The
// preconditions decide on or off; the checks only choose between enforced and degraded.
func ComputeStatus(pre Preconditions, failed []CheckResult) (Status, View) {
	v := View{Warnings: []string{}}
	switch {
	case !pre.P5:
		v.Status = Disabled
		v.Reason = EnvIsolation + "=off"
		return Disabled, v
	case !pre.DropPossible():
		v.Status = Unavailable
		var parts []string
		for _, id := range pre.Failing() {
			parts = append(parts, id+": "+pre.Reasons[id])
		}
		v.Reason = strings.Join(parts, "; ")
		return Unavailable, v
	}
	var why []string
	for _, c := range failed {
		line := c.ID + ": " + c.Detail
		v.Warnings = append(v.Warnings, line)
		if c.Degrades {
			why = append(why, line)
		}
	}
	if len(why) > 0 {
		v.Status = Degraded
		v.Reason = strings.Join(why, "; ")
		return Degraded, v
	}
	v.Status = Enforced
	return Enforced, v
}
