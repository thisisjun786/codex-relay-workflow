package goalplan

// CXC v0.2.40 goalplan.ts:915-1030. firstInvalidField (:875-913) is in read.go.
import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"golang.org/x/sys/unix"
)

// NewGoalplanCriterion seeds a criterion; nil Surface defaults to logic, while
// a pointer to an empty surface keeps the oracle's explicitly empty value.
type NewGoalplanCriterion struct {
	Scenario, ExpectedEvidence string
	Surface                    *CriterionSurface
	Presented                  PresentedSurface
}

// NewGoalplanInput builds a record without IO. Now is called exactly once.
type NewGoalplanInput struct {
	Objective     string
	Criteria      []NewGoalplanCriterion
	Host          *GoalplanHostLink
	SchemaVersion *float64
	Now           func() string
}

func writeTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
func writeNormalizeNewSchemaVersion(requested *float64) float64 {
	if requested == nil || math.IsNaN(*requested) || math.IsInf(*requested, 0) || math.Floor(*requested) < 1 {
		return DefaultNewSchemaVersion
	}
	return min(math.Floor(*requested), SupportedMaxSchemaVersion)
}

// BuildGoalplan ports :996-1025, including the fresh object's insertion order.
func BuildGoalplan(in NewGoalplanInput) *Goalplan {
	now := in.Now
	if now == nil {
		now = writeTimestamp
	}
	ts := now()
	version := writeNormalizeNewSchemaVersion(in.SchemaVersion)
	plan := &Goalplan{Objective: in.Objective, Slug: interview.DeriveSlug(in.Objective), SchemaVersion: &version, CreatedAt: ts, UpdatedAt: ts,
		WorkPhases: []GoalplanWorkPhase{}, Criteria: make([]GoalplanCriterion, 0, len(in.Criteria)), Host: GoalplanHostLink{Source: HostSourceNone}, goalplanBuiltFresh: true}
	for i, c := range in.Criteria {
		surface := SurfaceLogic
		if c.Surface != nil {
			surface = *c.Surface
		}
		criterion := GoalplanCriterion{ID: fmt.Sprintf("c-%d", i+1), Scenario: c.Scenario, Surface: surface, ExpectedEvidence: c.ExpectedEvidence, Status: CriterionOpen}
		if c.Presented == PresentedNative {
			criterion.Presented = PresentedNative
		}
		plan.Criteria = append(plan.Criteria, criterion)
	}
	if in.Host != nil {
		plan.Host.Armed, plan.Host.ArmedAt = in.Host.Armed, in.Host.ArmedAt
		if in.Host.Source == HostSourceFreeze {
			plan.Host.Source = HostSourceFreeze
		}
	}
	return plan
}

// Explicit fields shadow the embedded record; its remaining optional fields
// follow the fresh header. Revival discards the private construction provenance.
type writeFreshCriterion struct {
	ID               string           `json:"id"`
	Scenario         string           `json:"scenario"`
	Surface          CriterionSurface `json:"surface"`
	Presented        PresentedSurface `json:"presented,omitempty"`
	ExpectedEvidence string           `json:"expectedEvidence"`
	CapturedEvidence *string          `json:"capturedEvidence"`
	Status           CriterionStatus  `json:"status"`
}
type writeFreshPlan struct {
	Objective         string                `json:"objective"`
	Slug              string                `json:"slug"`
	SchemaVersion     *float64              `json:"schemaVersion,omitempty"`
	CreatedAt         string                `json:"createdAt"`
	UpdatedAt         string                `json:"updatedAt"`
	ActiveWorkPhaseID *string               `json:"activeWorkPhaseId"`
	WorkPhases        []GoalplanWorkPhase   `json:"workPhases"`
	Criteria          []writeFreshCriterion `json:"criteria"`
	Host              GoalplanHostLink      `json:"host"`
	Goalplan
}

func writeEncodePlan(plan Goalplan) ([]byte, error) {
	if !plan.goalplanBuiltFresh {
		return writeEncodeJSON(plan, "  ")
	}
	criteria := make([]writeFreshCriterion, len(plan.Criteria))
	for i, c := range plan.Criteria {
		criteria[i] = writeFreshCriterion{c.ID, c.Scenario, c.Surface, c.Presented, c.ExpectedEvidence, c.CapturedEvidence, c.Status}
	}
	return writeEncodeJSON(writeFreshPlan{plan.Objective, plan.Slug, plan.SchemaVersion, plan.CreatedAt, plan.UpdatedAt, plan.ActiveWorkPhaseID, plan.WorkPhases, criteria, plan.Host, plan}, "  ")
}

// The existing state.stringify and oracle test encoder are private. Preserve
// JSON.stringify's literal markup/U+2028/U+2029 and lack of a final newline.
func writeEncodeJSON(value any, indent string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	in := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return out, nil
}

// WriteGoalplan is low-level atomic publication (:915-942). New-plan creation
// may call directly; existing mutations MUST run inside WithGoalplanWriteLock.
// Only a shallow copy receives the refreshed timestamp, as in the oracle.
// A failure after the rename published the plan, so it is returned as a
// *state.PublishedError (CRW-744's type, CRW-793 here) and a caller that has a
// reconciling step still runs it. The one exception is a path-identity failure
// at the post-rename directory open: the plan directory moved or was replaced,
// so the plan is not at its path and the write returns a plain error instead
// (CRW-856).
func WriteGoalplan(cwd string, plan *Goalplan) error {
	return goalplanPublishedWriteGoalplan(cwd, plan, nil)
}

// goalplanPublishedOptions is the CRW-793 durability seam: the sync the write path performs on the
// staged plan and on the directory that holds it. A nil Sync is (*os.File).Sync, so a production call
// never carries the seam; a caller that passes one drives the published-but-unsynced path. It is an
// argument, never package state, so one test cannot fault another's write.
type goalplanPublishedOptions struct {
	Sync func(*os.File) error

	// AfterRename runs immediately after the Renameat that publishes the plan, before the
	// post-rename directory open. It is a test seam: production passes nil, so no call ever
	// carries it, and it is an argument rather than package state so one test cannot affect
	// another's write (CRW-856).
	AfterRename func()
	// OpenDir replaces the post-rename directory open that the fsync reads. Nil is the real
	// openAt(dir, ".", expected, unix.O_RDONLY|unix.O_DIRECTORY, true, 0). It is a test seam,
	// an argument rather than package state (CRW-856).
	OpenDir func(dir *os.File, expected string) (*os.File, error)
}

// goalplanPublishedOpenDir is the post-rename directory open, defaulting to the real one.
func goalplanPublishedOpenDir(o *goalplanPublishedOptions, dir *os.File, expected string) (*os.File, error) {
	if o != nil && o.OpenDir != nil {
		return o.OpenDir(dir, expected)
	}
	return openAt(dir, ".", expected, unix.O_RDONLY|unix.O_DIRECTORY, true, 0)
}

// goalplanPublishedSync is the configured sync, defaulting to the real one.
func goalplanPublishedSync(o *goalplanPublishedOptions) func(*os.File) error {
	if o != nil && o.Sync != nil {
		return o.Sync
	}
	return (*os.File).Sync
}

func goalplanPublishedWriteGoalplan(cwd string, plan *Goalplan, o *goalplanPublishedOptions) error {
	checked, err := GoalplanDir(cwd, plan.Slug)
	if err != nil {
		return err
	}
	dir, real, err := writeOpenCheckedDir(cwd, checked)
	if err != nil {
		return err
	}
	defer dir.Close()
	normalized := *plan
	normalized.UpdatedAt = writeTimestamp()
	data, err := writeEncodePlan(normalized)
	if err != nil {
		return err
	}
	return goalplanPublishedWritePublishAt(dir, real, data, o)
}

// AppendGoalplanLedger ports :945-963. Existing-plan callers append under their
// WithGoalplanWriteLock, without reacquiring it here. A new ledger needs no plan.
// The LF separator fixes the oracle's loss of an unterminated final record.
func AppendGoalplanLedger(cwd, slug string, entry GoalplanLedgerEntry) error {
	if _, err := ValidateGoalplanSlug(slug); err != nil {
		return err
	}
	if entry.Slug != slug {
		return errors.New("goalplan ledger entry slug does not match target slug")
	}
	checked, err := GoalplanDir(cwd, slug)
	if err != nil {
		return err
	}
	dir, real, err := writeOpenCheckedDir(cwd, checked)
	if err != nil {
		return err
	}
	defer dir.Close()
	data, err := writeEncodeJSON(entry, "")
	if err != nil {
		return err
	}
	return writeAppendAt(dir, real, append(data, '\n'))
}

// Split at the lexical check so a deterministic regression can replace a
// checked path before the descriptor walk. No pathname mutation follows it.
func writeOpenCheckedDir(cwd, checked string) (*os.File, string, error) {
	return writeOpenCheckedDirWithIgnore(cwd, checked, writeIgnoreAt)
}

func writeOpenCheckedDirWithIgnore(cwd, checked string, writeIgnore func(*os.File, string) error) (*os.File, string, error) {
	base, err := filepath.Abs(cwd)
	if err == nil {
		base, err = filepath.EvalSymlinks(base)
	}
	if err != nil {
		return nil, "", err
	}
	fd, err := unix.Open("/", directoryOpenFlags()|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	parent := os.NewFile(uintptr(fd), "/")
	expected := "/"
	for _, part := range strings.Split(strings.TrimPrefix(base, "/"), "/") {
		if part == "" {
			continue
		}
		expected = filepath.Join(expected, part)
		next, e := openAt(parent, part, expected, directoryOpenFlags(), true, 0)
		_ = parent.Close()
		if e != nil {
			return nil, "", e
		}
		parent = next
	}
	for i, part := range []string{crwdir.DirName, GoalplansSubdir, filepath.Base(checked)} {
		if err = boundFile(parent, expected, true); err != nil {
			parent.Close()
			return nil, "", err
		}
		path := filepath.Join(expected, part)
		mode := uint32(0700)
		if i == 0 {
			mode = 0777
		}
		err = unix.Mkdirat(int(parent.Fd()), part, mode)
		created := err == nil
		if err != nil && err != unix.EEXIST {
			parent.Close()
			return nil, "", err
		}
		next, e := openAt(parent, part, path, directoryOpenFlags(), true, 0)
		if e != nil {
			parent.Close()
			return nil, "", e
		}
		if created && i == 0 {
			if e = writeIgnore(next, path); e != nil {
				writeRemoveEmptyDir(parent, next, part, path)
				parent.Close()
				next.Close()
				return nil, "", e
			}
		}
		_ = parent.Close()
		parent, expected = next, path
	}
	return parent, expected, nil
}
func writeRemoveEmptyDir(parent, owned *os.File, name, path string) {
	if boundFile(parent, filepath.Dir(path), true) != nil || boundFile(owned, path, true) != nil {
		return
	}
	current, err := openAt(parent, name, path, directoryOpenFlags(), true, 0)
	if err != nil {
		return
	}
	defer current.Close()
	own, e := owned.Stat()
	info, err := current.Stat()
	if e == nil && err == nil && os.SameFile(own, info) {
		_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
	}
}

func writeIgnoreAt(dir *os.File, real string) error {
	file, err := openAt(dir, ".gitignore", filepath.Join(real, ".gitignore"), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, false, 0666)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = file.WriteString(crwdir.GitignoreText)
	return errors.Join(err, file.Close())
}

func writePublishAt(dir *os.File, real string, data []byte) error {
	return goalplanPublishedWritePublishAt(dir, real, data, nil)
}

func goalplanPublishedWritePublishAt(dir *os.File, real string, data []byte, o *goalplanPublishedOptions) error {
	sync := goalplanPublishedSync(o)
	if err := boundFile(dir, real, true); err != nil {
		return err
	}
	name := fmt.Sprintf("%s.%d.%s.tmp", GoalplanFile, os.Getpid(), rand.Text())
	file, err := openAt(dir, name, filepath.Join(real, name), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, false, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	_, err = file.Write(data)
	if err == nil {
		err = sync(file)
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = boundFile(dir, real, true); err != nil {
		return err
	}
	// POSIX renameWithRetry is one attempt. Renameat retains that behavior without
	// reopening a pathname that could have become a link since the check.
	if err = unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), GoalplanFile); err != nil {
		return err
	}
	if o != nil && o.AfterRename != nil {
		o.AfterRename()
	}
	reader, err := goalplanPublishedOpenDir(o, dir, real)
	// File fsync and rename remain available to a search/write-only directory,
	// as in the oracle. Directory fsync additionally runs when readable.
	if errors.Is(err, os.ErrPermission) {
		return nil
	}
	// A path-identity failure means the descriptor the plan was written into is no longer the
	// plan directory at its path: the directory was moved or replaced after the rename, so the
	// plan is not at the slug path and the write is not published. It is returned as a plain
	// error, not a PublishedError, so a caller with a reconciling step (steering) answers an
	// error and appends no ledger row, and a retry applies to the plan now at the path
	// (CRW-856). A genuine open failure (any other cause) still published the plan, so it
	// stays a PublishedError as before.
	if err != nil {
		var relocated *goalplanRelocatedError
		if errors.As(err, &relocated) {
			// filepath.Base(real) is the slug: writeOpenCheckedDir builds real as
			// <base>/.crw/goalplans/<slug> from the validated checked path.
			return fmt.Errorf("goalplan '%s' directory moved after publication; the plan at its path is not the one written", filepath.Base(real))
		}
		return &state.PublishedError{Err: err}
	}
	defer reader.Close()
	if err := sync(reader); err != nil {
		return &state.PublishedError{Err: err}
	}
	return nil
}
func writeAppendAt(dir *os.File, real string, data []byte) error {
	if err := boundFile(dir, real, true); err != nil {
		return err
	}
	file, err := openAt(dir, GoalplanLedgerFile, filepath.Join(real, GoalplanLedgerFile), unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND, false, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if writeLedgerNeedsLF(dir, real, file) {
		data = append([]byte{'\n'}, data...)
	}
	if err = boundFile(dir, real, true); err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	return err
}
func writeLedgerNeedsLF(dir *os.File, real string, appendFile *os.File) bool {
	info, err := appendFile.Stat()
	if err != nil {
		return true
	}
	if info.Size() == 0 {
		return false
	}
	reader, err := openAt(dir, GoalplanLedgerFile, filepath.Join(real, GoalplanLedgerFile), unix.O_RDONLY, false, 0)
	if err != nil {
		return true
	}
	defer reader.Close()
	current, err := reader.Stat()
	if err != nil || !os.SameFile(info, current) {
		return true
	}
	var tail [1]byte
	_, err = reader.ReadAt(tail[:], info.Size()-1)
	return err != nil || tail[0] != '\n'
}
