// Package cli holds PABCD command libraries, separate from their harness routing.
// The plan library ports CXC v0.2.40 pabcd-state/src/plan-cli.ts; command names
// follow contract/schema/cxc/name-substitution.json. No command is registered here.
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// PlanCliArgs is the parsed plan command. A nil Date stamps local today; a
// non-nil Date, including an empty string, preserves the caller's prefix.
type PlanCliArgs struct {
	Verb   string  `json:"verb"`
	Slug   string  `json:"slug"`
	Phases int     `json:"phases"`
	Cwd    string  `json:"cwd"`
	Date   *string `json:"date"`
}

// PlanCliResult carries the text and exit status without writing process streams.
type PlanCliResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

// YYMMDD formats the date in t's location, as the oracle's local Date getters do.
func YYMMDD(t time.Time) string {
	return fmt.Sprintf("%02d%02d%02d", t.Year()%100, t.Month(), t.Day())
}

// SplitDatePrefix recognizes six ASCII digits and either separator. Calendar
// validity is deliberately not checked; JavaScript's unflagged dot excludes
// all four line terminators from the remainder.
func SplitDatePrefix(raw string) (*string, string) {
	raw = text.Trim(raw)
	if len(raw) < 7 || (raw[6] != '_' && raw[6] != '-') || strings.ContainsAny(raw[7:], "\n\r\u2028\u2029") {
		return nil, raw
	}
	for _, b := range []byte(raw[:6]) {
		if b < '0' || b > '9' {
			return nil, raw
		}
	}
	date := raw[:6]
	return &date, raw[7:]
}

// DerivePlanSlug preserves underscores and trims hyphens before the ASCII
// 60-byte cut. U+0130 expands in JS lowercase rather than becoming plain i.
func DerivePlanSlug(raw string) string {
	lower := strings.ToLower(strings.ReplaceAll(text.Trim(raw), "\u0130", "i\u0307"))
	var b strings.Builder
	dash := false
	for _, r := range lower {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// ParsePlanCliArgs consumes argv without the plan token. Unknown double-dash
// tokens and later positionals are ignored, exactly as the oracle does.
func ParsePlanCliArgs(argv []string, cwd string) (PlanCliArgs, error) {
	args := PlanCliArgs{Verb: "help", Phases: 1, Cwd: cwd}
	if len(argv) == 0 {
		return args, nil
	}
	verb := strings.ToLower(strings.ReplaceAll(argv[0], "\u0130", "i\u0307"))
	if verb == "help" || verb == "--help" || verb == "-h" {
		return args, nil
	}
	if verb != "init" {
		return PlanCliArgs{}, fmt.Errorf("unknown plan verb '%s' (expected init)", argv[0])
	}
	slug := ""
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; a {
		case "--phases":
			i++
			if i >= len(argv) {
				return PlanCliArgs{}, errors.New("--phases expects an integer 1-9")
			}
			n, ok := planPhaseNumber(argv[i])
			if !ok {
				return PlanCliArgs{}, errors.New("--phases expects an integer 1-9")
			}
			args.Phases = n
		case "--cwd":
			i++
			if i < len(argv) {
				args.Cwd = argv[i]
			} else {
				args.Cwd = cwd
			}
		default:
			if !strings.HasPrefix(a, "--") && slug == "" {
				slug = a
			}
		}
	}
	if slug == "" {
		return PlanCliArgs{}, errors.New("plan init requires a <slug> argument")
	}
	date, slugRest := SplitDatePrefix(slug)
	args.Date = date
	args.Slug = DerivePlanSlug(slugRest)
	if args.Slug == "" {
		return PlanCliArgs{}, fmt.Errorf("plan init: '%s' has no usable slug once its date prefix is removed", slug)
	}
	args.Verb = "init"
	return args, nil
}

// Only JS Number strings whose value is an integer 1-9 matter at this boundary.
// ParseFloat alone accepts Go digit separators and hexadecimal floats, unlike JS.
func planPhaseNumber(raw string) (int, bool) {
	s := text.Trim(raw)
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			digits := s[2:]
			if strings.Contains(digits, "_") {
				return 0, false
			}
			n, err := strconv.ParseUint(digits, base, 64)
			return int(n), err == nil && n >= 1 && n <= 9
		}
	}
	if !regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`).MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 1 || n > 9 || n != float64(int(n)) {
		return 0, false
	}
	return int(n), true
}

// RunPlanCli creates one plan unit or refuses an existing one. Workspace
// selection follows Cwd (including its links); descendant symlink parents
// are refused. Root handles confine lookups and exclusive creation never
// truncates a pre-existing file. A partial new unit is retained after failure,
// as in the oracle. A privileged directory mover or mount is outside this guard.
func RunPlanCli(args PlanCliArgs) PlanCliResult { return runPlanCli(args, writePlanDoc) }

func runPlanCli(args PlanCliArgs, writeDoc func(*os.Root, string, string) error) PlanCliResult {
	if args.Verb == "help" {
		return PlanCliResult{Output: planHelp}
	}
	date := YYMMDD(time.Now())
	if args.Date != nil {
		date = *args.Date
	}
	name := date + "_" + args.Slug
	if !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\") {
		return planFailure(errors.New("plan unit must be a single workspace path component"))
	}
	cwd, err := filepath.Abs(args.Cwd)
	if err != nil {
		return planFailure(err)
	}
	unitDir := filepath.Join(cwd, "devlog", "_plan", name)
	refuse := func() PlanCliResult {
		return PlanCliResult{Code: 1, Output: fmt.Sprintf("plan init: %s already exists — refusing to overwrite. Write your docs there.", unitDir)}
	}
	parent, err := planParent(cwd, unitDir)
	if err != nil {
		return planFailure(err)
	}
	defer parent.Close()
	if _, err := parent.Lstat(name); err == nil {
		return refuse()
	} else if !errors.Is(err, os.ErrNotExist) {
		return planFailure(planPathError("mkdir", unitDir, err))
	}
	if err := parent.Mkdir(name, 0o777); err != nil {
		if errors.Is(err, os.ErrExist) {
			return refuse()
		}
		return planFailure(planPathError("mkdir", unitDir, err))
	}
	unit, err := openPlanChild(parent, name)
	if err != nil {
		return planFailure(err)
	}
	defer unit.Close()
	if err := writeDoc(unit, "000_plan.md", planDoc(args.Slug)); err != nil {
		return planFailure(err)
	}
	for n := 1; n <= args.Phases; n++ {
		doc := fmt.Sprintf("%03d_phase%d.md", n*10, n)
		if err := writeDoc(unit, doc, phaseDoc(n, args.Slug)); err != nil {
			return planFailure(err)
		}
	}
	return PlanCliResult{Output: fmt.Sprintf("plan init: scaffolded %s (000_plan.md + %d phase doc(s)).\nWrite every doc to diff-level BEFORE P -> A; the P>A gate requires planUnit to carry numbered docs.", filepath.Join("devlog", "_plan", name), args.Phases)}
}

func planParent(cwd, unitDir string) (*os.Root, error) {
	if err := os.MkdirAll(cwd, 0o777); err != nil {
		return nil, planPathError("mkdir", unitDir, err)
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, planPathError("mkdir", unitDir, err)
	}
	for _, part := range []string{"devlog", "_plan"} {
		if err := root.Mkdir(part, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
			root.Close()
			return nil, planPathError("mkdir", unitDir, err)
		}
		next, err := openPlanChild(root, part)
		root.Close()
		if err != nil {
			return nil, planPathError("mkdir", unitDir, err)
		}
		root = next
	}
	return root, nil
}

// Pin each non-link directory and check the opened inode against Lstat. A link
// swapped in before OpenRoot cannot redirect subsequent work to another inode.
func openPlanChild(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("plan parent must not be a symlink: %s", filepath.Join(parent.Name(), name))
	}
	if !info.IsDir() {
		return nil, syscall.ENOTDIR
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		child.Close()
		return nil, errors.New("plan directory changed while opening")
	}
	return child, nil
}

func writePlanDoc(root *os.Root, name, data string) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return planPathError("open", filepath.Join(root.Name(), name), err)
	}
	_, writeErr := f.WriteString(data)
	closeErr := f.Close()
	if writeErr != nil {
		return planPathError("write", "", writeErr)
	}
	if closeErr != nil {
		return planPathError("close", "", closeErr)
	}
	return nil
}

func planPathError(op, path string, err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return &os.PathError{Op: op, Path: path, Err: errno}
	}
	return err
}

func planFailure(err error) PlanCliResult {
	output := err.Error()
	var path *os.PathError
	var errno syscall.Errno
	if errors.As(err, &path) && errors.As(err, &errno) {
		name, desc := planErrno(errno)
		if name != "" {
			output = name + ": " + desc + ", " + path.Op
			if path.Op != "write" && path.Op != "close" {
				output += " '" + path.Path + "'"
			}
		}
	}
	return PlanCliResult{Code: 1, Output: "plan init failed: " + output}
}

// libuv's filesystem errno descriptions, exposed by the oracle's Error.message.
func planErrno(e syscall.Errno) (string, string) {
	switch e {
	case syscall.ENOENT:
		return "ENOENT", "no such file or directory"
	case syscall.EACCES:
		return "EACCES", "permission denied"
	case syscall.EPERM:
		return "EPERM", "operation not permitted"
	case syscall.ENOTDIR:
		return "ENOTDIR", "not a directory"
	case syscall.EEXIST:
		return "EEXIST", "file already exists"
	case syscall.EISDIR:
		return "EISDIR", "illegal operation on a directory"
	case syscall.ENOSPC:
		return "ENOSPC", "no space left on device"
	case syscall.EROFS:
		return "EROFS", "read-only file system"
	case syscall.EIO:
		return "EIO", "i/o error"
	case syscall.EMFILE:
		return "EMFILE", "too many open files"
	case syscall.ENFILE:
		return "ENFILE", "file table overflow"
	case syscall.ENAMETOOLONG:
		return "ENAMETOOLONG", "name too long"
	case syscall.ELOOP:
		return "ELOOP", "too many symbolic links encountered"
	}
	return "", ""
}

// Scaffold text is copied verbatim from the recorded Node documents.
func planDoc(slug string) string         { return fmt.Sprintf(planTemplate, slug) }
func phaseDoc(n int, slug string) string { return fmt.Sprintf(phaseTemplate, n*10, n, slug) }

const planTemplate = `# 000 — %s: Plan

> DIFFLEVEL-ROADMAP-01: write this doc to full diff-level precision (exact paths,
> NEW/MODIFY/DELETE, before/after diffs) BEFORE P -> A. An empty scaffold does not
> satisfy the rule; the A-phase reviewer FAILS outline-only phase docs.

## Objective

(fill in: the concrete outcome, the observed failure, the evidence base)

## Loop-spec

- Loop archetype: (verifier-defined | judged)
- Write scope / out-of-scope:
- Budget / bounds:

## Work-phase map (one phase = one full PABCD cycle)

| WP | Doc | Slice | Depends on |
|----|-----|-------|------------|

## Accept criteria

- (mirror into the goalplan criteria[])
`

const phaseTemplate = `# %03d — Phase %d (%s)

> DIFFLEVEL-ROADMAP-01: write this doc to full diff-level precision (exact paths,
> NEW/MODIFY/DELETE, before/after diffs) BEFORE P -> A. An empty scaffold does not
> satisfy the rule; the A-phase reviewer FAILS outline-only phase docs.

## MODIFY / NEW / DELETE map

(fill in: exact file paths with before/after diffs — a copy-paste-executable PRD)

## TESTS

(fill in: test files + cases)

## Verification (C)

(fill in: exact commands + expected exit codes)
`

const planHelp = `crw pabcd plan — scaffold a devlog plan unit (DIFFLEVEL-ROADMAP-01)

Usage:
  crw pabcd plan init --slug <slug> [--phases <n>] [--date <YYMMDD>] [--cwd <path>]
  crw pabcd plan --help

Notes:
  Creates devlog/_plan/<YYMMDD>_<slug>/ with 000_plan.md plus one decade doc
  (010, 020, ...) per phase. P>A requires such a unit to exist on disk with
  numbered docs — a chat-message plan does not satisfy Plan.
  --date is for callers that already carry their own prefix; omit it to stamp today.
  init refuses to overwrite an existing unit.`
