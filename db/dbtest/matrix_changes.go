package dbtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// The expected-changes file (docs/phase-d2-plan.md section 5.4) lists, per shape, every
// difference startup may make to the rows of a database. Only three kinds of entry exist:
// value, deleted, added. There is no whole-table and no whole-column entry. Every entry has a
// cause (file:line) and a class.

// Entry kinds.
const (
	KindValue   = "value"
	KindDeleted = "deleted"
	KindAdded   = "added"
)

// Entry classes. A defect entry names a difference the plan or the run found that nobody has
// decided on: it makes the comparison fail ("awaiting user decision") until the user fixes
// the code or reclassifies the entry. The implementer never picks intended or accepted alone.
// An accepted entry is a difference the owner decided to keep; it must carry a note that says
// so and why (a decision is recorded, not implied).
const (
	ClassIntended = "intended"
	ClassAccepted = "accepted"
	ClassDefect   = "defect"
)

// AfterStartup, as the after value of a value entry, stands for a timestamp that is not
// earlier than the moment startup began and not later than the database clock read after it
// (plus a small slack). It is the only way to state the result of a now()-valued column (the
// set_updated_at trigger): an exact value cannot be written down. It is valid for timestamptz
// columns only.
const AfterStartup = "@startup"

// ExpectedChange is one entry. Before and After keep the difference between an absent field
// and JSON null, because null is a value (SQL NULL) in a value entry.
type ExpectedChange struct {
	Kind        string            `json:"kind"`
	Table       string            `json:"table"`
	PK          map[string]string `json:"pk,omitempty"`
	Column      string            `json:"column,omitempty"`
	Before      json.RawMessage   `json:"before,omitempty"`
	After       json.RawMessage   `json:"after,omitempty"`
	ColumnAdded bool              `json:"column_added,omitempty"` // the column does not exist before startup
	Count       int               `json:"count,omitempty"`
	Cause       string            `json:"cause"`
	Class       string            `json:"class"`
	Note        string            `json:"note,omitempty"` // required for class accepted: the decision and its reason
}

// ExpectedFile is expected-changes.json.
type ExpectedFile struct {
	Note   string                      `json:"note"`
	Shapes map[string][]ExpectedChange `json:"shapes"`
}

// ReadExpectedChanges parses the file strictly (unknown fields are an error) and validates
// the shape of every entry.
func ReadExpectedChanges(r io.Reader) (*ExpectedFile, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read expected changes: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f ExpectedFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("expected changes: %w", err)
	}
	for shape, list := range f.Shapes {
		seen := map[string]int{}
		for i, c := range list {
			if err := c.Validate(); err != nil {
				return nil, fmt.Errorf("expected changes: shape %s entry %d: %w", shape, i+1, err)
			}
			id := c.identity()
			if j, dup := seen[id]; dup {
				return nil, fmt.Errorf("expected changes: shape %s entries %d and %d name the same change", shape, j+1, i+1)
			}
			seen[id] = i
		}
	}
	return &f, nil
}

var causePattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+:[0-9]+(-[0-9]+)?(; [A-Za-z0-9_./-]+:[0-9]+(-[0-9]+)?)*$`)

// Causes returns the file:line references of an entry's cause.
func (c ExpectedChange) Causes() []string { return strings.Split(c.Cause, "; ") }

// Validate checks the shape of one entry, not its truth.
func (c ExpectedChange) Validate() error {
	if c.Table == "" {
		return fmt.Errorf("no table")
	}
	if !causePattern.MatchString(c.Cause) {
		return fmt.Errorf("cause %q is not file:line (several: separated by \"; \")", c.Cause)
	}
	switch c.Class {
	case ClassIntended, ClassDefect:
	case ClassAccepted:
		if strings.TrimSpace(c.Note) == "" {
			return fmt.Errorf("an accepted entry needs a note that records the decision")
		}
	default:
		return fmt.Errorf("class %q is none of intended, accepted, defect", c.Class)
	}
	switch c.Kind {
	case KindValue:
		if len(c.PK) == 0 || c.Column == "" {
			return fmt.Errorf("a value entry needs pk and column (no whole-table or whole-column entries)")
		}
		if c.Count != 0 {
			return fmt.Errorf("a value entry has no count")
		}
		if len(c.After) == 0 {
			return fmt.Errorf("a value entry needs after")
		}
		if c.ColumnAdded {
			if len(c.Before) != 0 {
				return fmt.Errorf("column_added means there is no before value; omit before")
			}
		} else if len(c.Before) == 0 {
			return fmt.Errorf("a value entry needs before (JSON null is SQL NULL)")
		}
		if _, _, err := decodeValue(c.Before); err != nil {
			return fmt.Errorf("before: %w", err)
		}
		if _, _, err := decodeValue(c.After); err != nil {
			return fmt.Errorf("after: %w", err)
		}
	case KindDeleted:
		if len(c.PK) == 0 {
			return fmt.Errorf("a deleted entry needs pk")
		}
		if c.Column != "" || len(c.Before) != 0 || len(c.After) != 0 || c.Count != 0 || c.ColumnAdded {
			return fmt.Errorf("a deleted entry has only table and pk")
		}
	case KindAdded:
		// With a pk the entry names the one row that is added; without it, a number of rows.
		if len(c.PK) > 0 {
			if c.Count != 0 {
				return fmt.Errorf("an added entry with a pk names one row; omit count")
			}
		} else if c.Count < 1 {
			return fmt.Errorf("an added entry needs a pk (one row) or a count of at least 1")
		}
		if c.Column != "" || len(c.Before) != 0 || len(c.After) != 0 || c.ColumnAdded {
			return fmt.Errorf("an added entry has only table and pk, or table and count")
		}
	default:
		return fmt.Errorf("kind %q is none of value, deleted, added", c.Kind)
	}
	return nil
}

// decodeValue reads a JSON string or null. A missing field returns present=false.
func decodeValue(raw json.RawMessage) (val *string, present bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, fmt.Errorf("a value is a JSON string (the text form of the column) or null")
	}
	return &s, true, nil
}

func (c ExpectedChange) identity() string {
	keys := make([]string, 0, len(c.PK))
	for k, v := range c.PK {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return c.Kind + "|" + c.Table + "|" + strings.Join(keys, ",") + "|" + c.Column
}

// StartupCheck reports whether a timestamp text lies between the start of the startup being
// compared and the database clock read after it, plus a small slack (the AfterStartup marker).
type StartupCheck func(after string) (bool, error)

// CompareResult is the outcome of CompareRows. Problems are differences the file does not
// account for, or entries the run did not confirm. Awaiting lists confirmed entries of class
// defect. Neither holds a value.
type CompareResult struct {
	Problems []string
	Awaiting []string
}

// OK is true when nothing needs fixing and nothing awaits a decision.
func (r CompareResult) OK() bool { return len(r.Problems) == 0 && len(r.Awaiting) == 0 }

// Report renders the result for a failure message.
func (r CompareResult) Report() string {
	var b strings.Builder
	if len(r.Problems) > 0 {
		fmt.Fprintf(&b, "%d difference(s) between the rows before and after startup that expected-changes.json does not account for:\n  %s\n",
			len(r.Problems), strings.Join(r.Problems, "\n  "))
	}
	if len(r.Awaiting) > 0 {
		fmt.Fprintf(&b, "%d entr(y/ies) of class defect confirmed by the run, awaiting user decision (fix the code, or the user reclassifies the entry):\n  %s\n",
			len(r.Awaiting), strings.Join(r.Awaiting, "\n  "))
	}
	return b.String()
}

// CompareRows checks AC3: every row present before is present after, every value is
// unchanged, and rows are added only as stated, except for exactly the listed entries. An
// entry is matched on kind, table, primary key and column, and must also state the exact
// value before and after (AfterStartup for a trigger-maintained timestamp). An added entry
// with a pk names that row; one with a count covers the added rows no pk entry names, and
// their number must equal the count. Every entry must be confirmed by the run; a stale entry
// is a problem.
func CompareRows(before, after *RowSnapshot, changes []ExpectedChange, startup StartupCheck) CompareResult {
	var res CompareResult
	problem := func(format string, a ...any) { res.Problems = append(res.Problems, fmt.Sprintf(format, a...)) }

	used := make([]bool, len(changes))
	byID := map[string]int{}
	addedByTable := map[string]int{} // count entries: the rows no pk entry names
	addedByKey := map[string]int{}   // pk entries: table|row key
	for i, c := range changes {
		switch {
		case c.Kind == KindAdded && len(c.PK) == 0:
			addedByTable[c.Table] = i
		default:
			t := metaOf(before, after, c.Table)
			if t == nil {
				problem("entry %d (%s %s): the table does not exist before or after startup", i+1, c.Kind, c.Table)
				used[i] = true
				continue
			}
			key, err := t.KeyFromPK(c.PK)
			if err != nil {
				problem("entry %d (%s %s): %v", i+1, c.Kind, c.Table, err)
				used[i] = true
				continue
			}
			if c.Kind == KindAdded {
				addedByKey[c.Table+"|"+key] = i
			} else {
				byID[c.Kind+"|"+c.Table+"|"+key+"|"+c.Column] = i
			}
		}
	}
	awaiting := func(i int) {
		c := changes[i]
		if c.Class == ClassDefect {
			what := c.Kind + " " + c.Table
			if c.Column != "" {
				what += "." + c.Column
			}
			if len(c.PK) > 0 {
				if t := metaOf(before, after, c.Table); t != nil {
					if key, err := t.KeyFromPK(c.PK); err == nil {
						what += " " + t.DescribeKey(key)
					}
				}
			}
			if c.Kind == KindAdded && c.Count > 0 {
				what += fmt.Sprintf(" +%d", c.Count)
			}
			res.Awaiting = append(res.Awaiting, fmt.Sprintf("entry %d (%s; cause %s): awaiting user decision", i+1, what, c.Cause))
		}
	}

	added := map[string]int{}
	for _, d := range DiffRows(before, after) {
		switch d.Kind {
		case DiffAdded:
			if i, ok := addedByKey[d.Table+"|"+d.Key]; ok {
				used[i] = true
				awaiting(i)
				continue
			}
			added[d.Table]++
		case DiffColumnRemoved:
			problem("%s", d.Describe())
		case DiffDeleted:
			i, ok := byID[KindDeleted+"|"+d.Table+"|"+d.Key+"|"]
			if !ok {
				problem("%s, no entry", d.Describe())
				continue
			}
			used[i] = true
			awaiting(i)
		case DiffValue:
			i, ok := byID[KindValue+"|"+d.Table+"|"+d.Key+"|"+d.Column]
			if !ok {
				problem("%s, no entry", d.Describe())
				continue
			}
			used[i] = true
			c := changes[i]
			ok = true
			if c.ColumnAdded != d.ColumnAdded {
				problem("entry %d (%s): column_added is %v in the entry and %v in the run", i+1, d.Describe(), c.ColumnAdded, d.ColumnAdded)
				ok = false
			}
			if !c.ColumnAdded {
				if want, _, _ := decodeValue(c.Before); !sameValue(want, d.Before) {
					problem("entry %d (%s): the value before differs from the entry", i+1, d.Describe())
					ok = false
				}
			}
			want, _, _ := decodeValue(c.After)
			switch {
			case want != nil && *want == AfterStartup:
				if t := metaOf(before, after, d.Table); t == nil || !t.IsTimestamp(d.Column) {
					problem("entry %d (%s): %s is valid for a timestamptz column only", i+1, d.Describe(), AfterStartup)
					ok = false
				} else if d.After == nil {
					problem("entry %d (%s): the value after is NULL, the entry says a timestamp from startup", i+1, d.Describe())
					ok = false
				} else if startup == nil {
					problem("entry %d (%s): no startup check available", i+1, d.Describe())
					ok = false
				} else if fresh, err := startup(*d.After); err != nil {
					problem("entry %d (%s): startup check failed: %v", i+1, d.Describe(), err)
					ok = false
				} else if !fresh {
					problem("entry %d (%s): the value after is not a timestamp from startup", i+1, d.Describe())
					ok = false
				}
			case !sameValue(want, d.After):
				problem("entry %d (%s): the value after differs from the entry", i+1, d.Describe())
				ok = false
			}
			if ok {
				awaiting(i)
			}
		}
	}

	tables := map[string]bool{}
	for t := range added {
		tables[t] = true
	}
	for t := range addedByTable {
		tables[t] = true
	}
	names := make([]string, 0, len(tables))
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		got := added[t]
		i, listed := addedByTable[t]
		switch {
		case !listed:
			problem("table %s: %d row(s) added, no entry", t, got)
		case changes[i].Count != got:
			used[i] = true
			problem("entry %d (added %s): %d row(s) added, the entry says %d", i+1, t, got, changes[i].Count)
		default:
			used[i] = true
			awaiting(i)
		}
	}

	for i, c := range changes {
		if !used[i] {
			what := c.Kind + " " + c.Table
			if c.Column != "" {
				what += "." + c.Column
			}
			problem("entry %d (%s) was not confirmed by the run", i+1, what)
		}
	}
	sort.Strings(res.Problems)
	sort.Strings(res.Awaiting)
	return res
}

func metaOf(before, after *RowSnapshot, table string) *TableRows {
	if t := after.Tables[table]; t != nil {
		return t
	}
	return before.Tables[table]
}
