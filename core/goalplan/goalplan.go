// Package goalplan implements the goal/plan documents (P2b-M1/M2):
// Markdown + frontmatter files in the config store, a strict state
// machine, and the agent tools that drive them.
//
// Layout (docs/design/p2b.md §1.1/§1.2):
//
//	<store>/goals/<date>-<slug>.md
//	<store>/plans/<date>-<slug>.md
//
// Cross-file references use ids only. Files carry
//
//	---
//	kind: goal | plan
//	id: g-<rand> | p-<rand>
//	session: <creating session>
//	status: active | done | blocked | archived
//	created: RFC3339
//	updated: RFC3339
//	goal: <id>        (plan only)
//	---
//	body
//
// State machine (§1.3):
//
//	goal: active -> done | blocked -> archived; blocked -> active needs
//	      user confirmation through the permission service (kind
//	      goal_unblock; timeout or denial keeps blocked; channel
//	      sources are refused as read-only).
//	plan: active -> done (all linked todos checked) | archived.
//
// blocked requires a reason as the first body line: "blocked: <why>".
//
// Frontmatter is protected: write_file/edit_file/run_shell redirection
// into it is refused (the guard's protected set covers the files; the
// tools' confirm path refuses frontmatter edits). Bodies are editable.
package goalplan

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Status values (§1.3).
const (
	StatusActive   = "active"
	StatusDone     = "done"
	StatusBlocked  = "blocked"
	StatusArchived = "archived"
)

// Kinds.
const (
	KindGoal = "goal"
	KindPlan = "plan"
)

// Errors: stable text so callers and tests can match.
var (
	// ErrNotFound: the id does not exist.
	ErrNotFound = errors.New("goalplan: id not found")
	// ErrInvalidTransition: the state machine refuses the move.
	ErrInvalidTransition = errors.New("goalplan: invalid status transition")
	// ErrBlockedNeedsReason: blocked without a reason line.
	ErrBlockedNeedsReason = errors.New("goalplan: blocked requires a reason (first body line: blocked: <why>)")
	// ErrOpenTodos: plan -> done with unchecked todos.
	ErrOpenTodos = errors.New("goalplan: plan has unchecked todos; cannot set done")
	// ErrBadFilename: slug/filename malformed.
	ErrBadFilename = errors.New("goalplan: invalid file name")
	// ErrFrontmatter: refusing to mutate the frontmatter.
	ErrFrontmatter = errors.New("goalplan: frontmatter is protected")
)

// Doc is one parsed goal/plan document.
type Doc struct {
	Kind     string    `yaml:"kind"`
	ID       string    `yaml:"id"`
	Session  string    `yaml:"session"`
	Status   string    `yaml:"status"`
	Created  time.Time `yaml:"created"`
	Updated  time.Time `yaml:"updated"`
	Goal     string    `yaml:"goal,omitempty"` // plan only
	File     string    `yaml:"-"`              // absolute path
	Body     string    `yaml:"-"`
	FileName string    `yaml:"-"` // <date>-<slug>.md
}

// frontmatter is the on-disk subset (no body/file).
type frontmatter struct {
	Kind    string    `yaml:"kind"`
	ID      string    `yaml:"id"`
	Session string    `yaml:"session"`
	Status  string    `yaml:"status"`
	Created time.Time `yaml:"created"`
	Updated time.Time `yaml:"updated"`
	Goal    string    `yaml:"goal,omitempty"`
}

// blockedPrefix is the required first line when status=blocked.
const blockedPrefix = "blocked: "

// Manager owns one store's goals/ and plans/ trees.
type Manager struct {
	Dir string // store root (~/.owltide)
	Now func() time.Time
}

// New builds a Manager.
func New(dir string) *Manager {
	return &Manager{Dir: dir, Now: time.Now}
}

func (m *Manager) subdir(kind string) string {
	if kind == KindGoal {
		return "goals"
	}
	return "plans"
}

// newID produces g-<8hex> / p-<8hex>.
func newID(kind string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	prefix := "g"
	if kind == KindPlan {
		prefix = "p"
	}
	return prefix + "-" + hex.EncodeToString(b)
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var filenameRe = regexp.MustCompile(`^\d{8}-[a-z0-9][a-z0-9-]*\.md$`)

// ValidateFilename checks <date>-<slug>.md.
func ValidateFilename(name string) error {
	if !filenameRe.MatchString(name) {
		return fmt.Errorf("%w: %q (want <YYYYMMDD>-<slug>.md, slug [a-z0-9-])", ErrBadFilename, name)
	}
	return nil
}

// Render produces the full document (exported: context_refs inject
// the rendered text into sub-agent tasks).
func (d Doc) Render() string {
	fm := frontmatter{
		Kind: d.Kind, ID: d.ID, Session: d.Session,
		Status: d.Status, Created: d.Created, Updated: d.Updated, Goal: d.Goal,
	}
	data, _ := yaml.Marshal(fm)
	return "---\n" + string(data) + "---\n" + strings.TrimRight(d.Body, "\n") + "\n"
}

// parse splits a document into Doc.
func parse(file, fileName, text string) (Doc, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "---") {
		return Doc{}, fmt.Errorf("%w: missing frontmatter in %s", ErrFrontmatter, fileName)
	}
	rest := strings.TrimPrefix(text, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Doc{}, fmt.Errorf("%w: unclosed frontmatter in %s", ErrFrontmatter, fileName)
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return Doc{}, fmt.Errorf("goalplan: bad frontmatter in %s: %w", fileName, err)
	}
	body := strings.TrimSpace(rest[end+4:])
	return Doc{
		Kind: fm.Kind, ID: fm.ID, Session: fm.Session, Status: fm.Status,
		Created: fm.Created, Updated: fm.Updated, Goal: fm.Goal,
		File: file, Body: body, FileName: fileName,
	}, nil
}

// Create writes a new document (status starts active). The caller
// supplies the slug; the file name is <today>-<slug>.md in the kind's
// subdirectory. Writes are atomic (temp file + rename).
func (m *Manager) Create(kind, slug, session, body, goal string) (Doc, error) {
	if !slugRe.MatchString(slug) {
		return Doc{}, fmt.Errorf("%w: slug %q", ErrBadFilename, slug)
	}
	if kind != KindGoal && kind != KindPlan {
		return Doc{}, fmt.Errorf("goalplan: kind must be goal or plan, got %q", kind)
	}
	if kind == KindPlan && goal == "" {
		return Doc{}, fmt.Errorf("goalplan: plan requires a goal id")
	}
	now := m.Now()
	name := now.Format("20060102") + "-" + slug + ".md"
	if err := ValidateFilename(name); err != nil {
		return Doc{}, err
	}
	dir := filepath.Join(m.Dir, m.subdir(kind))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Doc{}, err
	}
	file := filepath.Join(dir, name)
	if _, err := os.Stat(file); err == nil {
		return Doc{}, fmt.Errorf("goalplan: %s already exists", name)
	}
	d := Doc{
		Kind: kind, ID: newID(kind), Session: session, Status: StatusActive,
		Created: now, Updated: now, Goal: goal, Body: strings.TrimSpace(body),
		File: file, FileName: name,
	}
	if err := atomicWrite(file, []byte(d.Render())); err != nil {
		return Doc{}, err
	}
	return d, nil
}

// atomicWrite writes via a UNIQUE temp file in the same directory +
// rename, so a crash never leaves a half-written file and concurrent
// writers of the same target never collide on the temp name.
func atomicWrite(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, file)
}

// Load reads one document by id (scans both trees; ids are unique).
func (m *Manager) Load(id string) (Doc, error) {
	docs, err := m.List("")
	if err != nil {
		return Doc{}, err
	}
	for _, d := range docs {
		if d.ID == id {
			return d, nil
		}
	}
	return Doc{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// List returns documents of one kind ("" = both), sorted by file name.
func (m *Manager) List(kind string) ([]Doc, error) {
	kinds := []string{KindGoal, KindPlan}
	if kind != "" {
		kinds = []string{kind}
	}
	var out []Doc
	for _, k := range kinds {
		dir := filepath.Join(m.Dir, m.subdir(k))
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			d, err := parse(filepath.Join(dir, e.Name()), e.Name(), string(data))
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FileName < out[j].FileName })
	return out, nil
}

// Save writes a modified doc back (atomic; Updated refreshed).
func (m *Manager) Save(d Doc) error {
	if err := ValidateFilename(d.FileName); err != nil {
		return err
	}
	d.Updated = m.Now()
	return atomicWrite(d.File, []byte(d.Render()))
}

// SetStatus moves a document through the state machine. It returns
// (doc, userConfirmationNeeded, error):
//
//   - goal: active -> done | blocked (reason required) -> archived
//   - plan: active -> done (all todos checked) | archived
//   - blocked -> active is NOT applied here: it needs user
//     confirmation, so it returns a request for the caller to route
//     through the permission service (RequestUnblock).
func (m *Manager) SetStatus(id, to string, todoChecked func(planID string) (bool, error)) (Doc, string, bool, error) {
	d, err := m.Load(id)
	if err != nil {
		return Doc{}, "", false, err
	}
	from := d.Status
	if from == to {
		return d, from, false, fmt.Errorf("%w: %s is already %s", ErrInvalidTransition, id, to)
	}

	// blocked -> active: user confirmation required (never applied here).
	if from == StatusBlocked && to == StatusActive {
		return d, from, true, nil
	}

	switch d.Kind {
	case KindGoal:
		switch {
		case from == StatusActive && to == StatusDone:
		case from == StatusActive && to == StatusBlocked:
			if !strings.HasPrefix(strings.TrimSpace(d.Body), blockedPrefix) {
				return Doc{}, from, false, ErrBlockedNeedsReason
			}
		case from == StatusDone && to == StatusArchived:
		case from == StatusBlocked && to == StatusArchived:
		default:
			return Doc{}, from, false, fmt.Errorf("%w: goal %s: %s -> %s", ErrInvalidTransition, id, from, to)
		}
	case KindPlan:
		switch {
		case from == StatusActive && to == StatusDone:
			if todoChecked != nil {
				all, err := todoChecked(id)
				if err != nil {
					return Doc{}, from, false, err
				}
				if !all {
					return Doc{}, from, false, ErrOpenTodos
				}
			}
		case from == StatusActive && to == StatusArchived:
		default:
			return Doc{}, from, false, fmt.Errorf("%w: plan %s: %s -> %s", ErrInvalidTransition, id, from, to)
		}
	default:
		return Doc{}, from, false, fmt.Errorf("goalplan: unknown kind %q", d.Kind)
	}

	d.Status = to
	if err := m.Save(d); err != nil {
		return Doc{}, from, false, err
	}
	return d, from, false, nil
}

// ApplyUnblock finishes the user-confirmed blocked -> active move.
func (m *Manager) ApplyUnblock(id string) (Doc, error) {
	d, err := m.Load(id)
	if err != nil {
		return Doc{}, err
	}
	if d.Kind != KindGoal || d.Status != StatusBlocked {
		return Doc{}, fmt.Errorf("%w: %s is %s/%s (want blocked goal)", ErrInvalidTransition, id, d.Kind, d.Status)
	}
	// Drop the stale reason line.
	body := strings.TrimSpace(d.Body)
	if strings.HasPrefix(body, blockedPrefix) {
		body = strings.TrimSpace(strings.TrimPrefix(body, blockedPrefix))
		if i := strings.Index(body, "\n"); i >= 0 {
			body = body[i+1:]
		} else {
			body = ""
		}
	}
	d.Body = body
	d.Status = StatusActive
	if err := m.Save(d); err != nil {
		return Doc{}, err
	}
	return d, nil
}

// UncheckedTodos scans a plan body for "- [ ]" items.
func UncheckedTodos(planBody string) int {
	n := 0
	for _, line := range strings.Split(planBody, "\n") {
		if strings.Contains(line, "- [ ]") {
			n++
		}
	}
	return n
}

// CheckAllTodos reports whether every "- [ ]" in the plan body is
// checked (or the plan has none). todoChecked may override.
func TodosAllDone(planBody string) bool { return UncheckedTodos(planBody) == 0 }

// FrontmatterSpan returns the byte span of the frontmatter within
// text (for the protected-region check). ok=false when there is none.
func FrontmatterSpan(text string) (start, end int, ok bool) {
	if !strings.HasPrefix(text, "---") {
		return 0, 0, false
	}
	if i := strings.Index(text[3:], "\n---"); i >= 0 {
		return 0, 3 + i + 4, true
	}
	return 0, 0, false
}

// RefFromBody extracts the goal:/plan: reference ids mentioned in a
// text body (order of appearance, deduplicated).
func RefsFromBody(body string) (goals, plans []string) {
	seen := map[string]bool{}
	add := func(list []string, id string) []string {
		if id == "" || seen[id] {
			return list
		}
		seen[id] = true
		return append(list, id)
	}
	for _, m := range refRe.FindAllStringSubmatch(body, -1) {
		switch m[1] {
		case "goal":
			goals = add(goals, m[2])
		case "plan":
			plans = add(plans, m[2])
		}
	}
	return goals, plans
}

var refRe = regexp.MustCompile(`\b(goal|plan):([gp]-[0-9a-f]{8})\b`)

// ParseSeq extracts the numeric part of a req-<n> id (diagnostics).
func ParseSeq(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "req-"))
	return n
}
