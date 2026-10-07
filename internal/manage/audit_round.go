package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// The states one package of a round carries: pending before it is audited, audited once
// the grader answered with a usable result, and failed when it answered with anything
// else, so the next pass of the round picks it up again.
const (
	auditRoundPending = "pending"
	auditRoundAudited = "audited"
	auditRoundFailed  = "failed"
)

// auditRoundPackage is one package of a round: what it is, where it stands, and the last
// graded result. History keeps the results a re-audit replaced, oldest first.
type auditRoundPackage struct {
	Package   string               `json:"package"`
	State     string               `json:"state"`
	Head      string               `json:"head,omitempty"`
	AuditedAt string               `json:"audited_at,omitempty"`
	Status    string               `json:"status,omitempty"`
	P0        int                  `json:"p0,omitempty"`
	P1        int                  `json:"p1,omitempty"`
	P2        int                  `json:"p2,omitempty"`
	P3        int                  `json:"p3,omitempty"`
	Bundle    string               `json:"bundle,omitempty"`
	History   []auditRoundSnapshot `json:"history,omitempty"`
}

// auditRoundSnapshot is one replaced result, in the same shape as the current one.
type auditRoundSnapshot struct {
	Head      string `json:"head"`
	AuditedAt string `json:"audited_at"`
	Status    string `json:"status"`
	P0        int    `json:"p0"`
	P1        int    `json:"p1"`
	P2        int    `json:"p2"`
	P3        int    `json:"p3"`
	Bundle    string `json:"bundle"`
}

// auditRoundFile is one round: its name, when it started, and its packages.
type auditRoundFile struct {
	Round     string              `json:"round"`
	StartedAt string              `json:"started_at"`
	Packages  []auditRoundPackage `json:"packages"`
}

// auditRoundStatus is what round status prints: the counts and the one verdict that says
// whether the round met its exit condition.
type auditRoundStatus struct {
	Round   string `json:"round"`
	Total   int    `json:"total"`
	Audited int    `json:"audited"`
	Pending int    `json:"pending"`
	Failed  int    `json:"failed"`
	P0      int    `json:"p0"`
	P1      int    `json:"p1"`
	Clean   bool   `json:"clean"`
}

// auditRoundDir is where the round files live, below the audit state directory.
func auditRoundDir(e *Env, cfg *Config) string {
	return filepath.Join(auditStateDir(e, cfg), "audit", "rounds")
}

// auditRoundName accepts a round name that is a plain file name, so a name can never make
// a round read or write a file outside the rounds directory.
func auditRoundName(name string) error {
	if name == "" {
		return errors.New("the round name is empty")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") || filepath.Base(name) != name {
		return fmt.Errorf("round name %q is not a plain name", name)
	}
	return nil
}

// auditRoundPath is the round's file.
func auditRoundPath(e *Env, cfg *Config, name string) (string, error) {
	if err := auditRoundName(name); err != nil {
		return "", err
	}
	return filepath.Join(auditRoundDir(e, cfg), name+".json"), nil
}

// auditRoundLock takes the round's lock for the whole read-modify-write of one round, so
// two processes cannot audit one round at once. It is non-blocking: a second caller is
// refused by name rather than waiting. The lock file itself is never removed, because
// another process may hold it.
func auditRoundLock(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := strings.TrimSuffix(path, ".json") + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("round_locked: %s is held by another process", filepath.Base(lockPath))
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// auditRoundLoad reads a round file. A document that is not a JSON object, and a round that
// names no round, are refused. Decoding straight into the struct is not enough: JSON null
// unmarshals into it without an error and leaves the zero value, which would read as a whole
// round with no name, no start and no package, and be reported as a round that was read.
func auditRoundLoad(path string) (*auditRoundFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("round %s: %w", path, err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, fmt.Errorf("round %s: not a round document", path)
	}
	var doc auditRoundFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("round %s: %w", path, err)
	}
	if doc.Round == "" {
		return nil, fmt.Errorf("round %s: not a round document", path)
	}
	return &doc, nil
}

// auditRoundSave writes a round file atomically: a temporary file beside it, fsynced, then
// renamed over it, so a reader never sees a half-written round.
func auditRoundSave(path string, doc *auditRoundFile) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// auditRoundStart writes a new round with every package pending. A round file that already
// exists is refused rather than replaced: it holds the results and the history of a round
// that may still be running, and a repeated start command must not erase them.
func auditRoundStart(e *Env, cfg *Config, name string, packages []string) (*auditRoundFile, error) {
	path, err := auditRoundPath(e, cfg, name)
	if err != nil {
		return nil, err
	}
	if len(packages) == 0 {
		return nil, errors.New("the round names no package")
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("round_exists: %s already holds a round; use round status, or start a new name", filepath.Base(path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	doc := &auditRoundFile{Round: name, StartedAt: e.Now().UTC().Format(auditTimeFormat)}
	for _, pkg := range auditRoundUnique(packages) {
		doc.Packages = append(doc.Packages, auditRoundPackage{Package: pkg, State: auditRoundPending})
	}
	if err := auditRoundSave(path, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// auditRoundUnique removes repeated package names, keeping the first occurrence's order.
// A repeated name would leave a second entry that auditRoundApply never touches, so the
// round could never become clean.
func auditRoundUnique(packages []string) []string {
	seen := make(map[string]bool, len(packages))
	out := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
	}
	return out
}

// auditRoundApply writes one graded result into a round. The previous result, when there
// is one, moves into the package's history rather than being lost. A result whose status
// is not ok leaves the package failed, so the next pass picks it up again.
func auditRoundApply(doc *auditRoundFile, pkg, head, bundle string, result AuditResult) {
	index := -1
	for i := range doc.Packages {
		if doc.Packages[i].Package == pkg {
			index = i
			break
		}
	}
	if index < 0 {
		doc.Packages = append(doc.Packages, auditRoundPackage{Package: pkg})
		index = len(doc.Packages) - 1
	}
	entry := &doc.Packages[index]
	if entry.State == auditRoundAudited || entry.State == auditRoundFailed {
		entry.History = append(entry.History, auditRoundSnapshot{
			Head: entry.Head, AuditedAt: entry.AuditedAt, Status: entry.Status,
			P0: entry.P0, P1: entry.P1, P2: entry.P2, P3: entry.P3, Bundle: entry.Bundle,
		})
	}
	p0, p1, p2, p3 := auditCounts(result.Defects)
	entry.Head = head
	entry.AuditedAt = result.GradedAt
	entry.Status = result.Status
	entry.P0, entry.P1, entry.P2, entry.P3 = p0, p1, p2, p3
	entry.Bundle = bundle
	if result.Status == auditStatusOK {
		entry.State = auditRoundAudited
	} else {
		entry.State = auditRoundFailed
	}
}

// auditRoundOutstanding is the packages a round still owes a result for, in file order: the
// ones never audited, the ones whose audit failed, and the ones whose last audit reported a
// P0 or a P1. That last group is what makes the round reach clean: a defect that was fixed
// is re-audited, and its replaced result moves into the package's history.
func auditRoundOutstanding(doc *auditRoundFile) []string {
	var out []string
	for _, entry := range doc.Packages {
		if entry.State == auditRoundPending || entry.State == auditRoundFailed || entry.P0+entry.P1 > 0 {
			out = append(out, entry.Package)
		}
	}
	return out
}

// auditRoundStatusOf is the round's progress. clean means every package was audited with
// an ok status and no package carries a P0 or a P1.
func auditRoundStatusOf(doc *auditRoundFile) auditRoundStatus {
	status := auditRoundStatus{Round: doc.Round, Total: len(doc.Packages)}
	for _, entry := range doc.Packages {
		switch entry.State {
		case auditRoundAudited:
			status.Audited++
		case auditRoundFailed:
			status.Failed++
		default:
			status.Pending++
		}
		status.P0 += entry.P0
		status.P1 += entry.P1
	}
	status.Clean = status.Pending == 0 && status.Failed == 0 && status.P0+status.P1 == 0 && status.Total > 0
	return status
}

// auditRoundWriteStatus prints the round's progress as JSON.
func auditRoundWriteStatus(e *Env, doc *auditRoundFile) int {
	data, err := json.Marshal(auditRoundStatusOf(doc))
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit round: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	return 0
}

// auditRoundUsage is the line the round subcommand prints.
const auditRoundUsage = "usage: crw manage audit round {start,status} --name R"

// auditRunRound is crw manage audit round.
func auditRunRound(ctx context.Context, e *Env, args []string) int {
	return auditRoundRun(ctx, e, args)
}

// auditRoundRun is crw manage audit round.
func auditRoundRun(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, auditRoundUsage)
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(e.Stdout, auditRoundUsage)
		return 0
	case "start":
		return auditRoundRunStart(ctx, e, args[1:])
	case "status":
		return auditRoundRunStatus(ctx, e, args[1:])
	}
	fmt.Fprintln(e.Stderr, auditRoundUsage)
	fmt.Fprintf(e.Stderr, "crw manage audit round: error: invalid command %q (choose from 'start', 'status')\n", args[0])
	return usageExit
}

// auditRoundStartUsage is the start subcommand's line. The head belongs to the pass that
// audits a package, not to the round that lists them.
const auditRoundStartUsage = "usage: crw manage audit round start --name R (--package DIR | --packages PATTERN)"

// auditRoundRunStart is crw manage audit round start.
func auditRoundRunStart(ctx context.Context, e *Env, args []string) int {
	values, multi, err := auditRoundParseStart(args)
	if err != nil {
		fmt.Fprintln(e.Stderr, auditRoundStartUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
		return usageExit
	}
	cfg := coreDefaults(e)
	co, err := auditPkgCheckoutOf(cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
		return 1
	}
	packages := multi["package"]
	if pattern := values["packages"]; pattern != "" {
		if co.Repository == "" {
			fmt.Fprintln(e.Stderr, "crw manage audit round start: error: checkout_unconfigured: the checkout section names no repository")
			return 1
		}
		out, err := auditPkgGoList(ctx, co.Repository, pattern)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
			return 1
		}
		expanded, err := auditPkgRelative(co, auditPkgLines(out))
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
			return 1
		}
		packages = append(packages, expanded...)
	}
	sort.Strings(packages)
	path, err := auditRoundPath(e, cfg, values["name"])
	if err != nil {
		fmt.Fprintln(e.Stderr, auditRoundStartUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
		return usageExit
	}
	release, err := auditRoundLock(path)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
		return 1
	}
	defer release()
	doc, err := auditRoundStart(e, cfg, values["name"], packages)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit round start: error: %v\n", err)
		return 1
	}
	return auditRoundWriteStatus(e, doc)
}

// auditRoundParseStart reads the start flags: the name, the repeated package directory and
// the go list pattern.
func auditRoundParseStart(args []string) (map[string]string, map[string][]string, error) {
	values, multi := map[string]string{}, map[string][]string{}
	for i := 0; i < len(args); i++ {
		name := args[i]
		if !strings.HasPrefix(name, "--") {
			return nil, nil, fmt.Errorf("unexpected argument %q", name)
		}
		key, value := strings.TrimPrefix(name, "--"), ""
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, value = key[:eq], key[eq+1:]
		} else {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, nil, fmt.Errorf("the option %s needs a value", name)
			}
			value = args[i]
		}
		switch key {
		case "name", "packages":
			values[key] = value
		case "package":
			multi[key] = append(multi[key], value)
		default:
			return nil, nil, fmt.Errorf("unknown option %s", name)
		}
	}
	if values["name"] == "" {
		return nil, nil, errors.New("--name is required")
	}
	if len(multi["package"]) == 0 && values["packages"] == "" {
		return nil, nil, errors.New("--package or --packages is required")
	}
	return values, multi, nil
}

// auditRoundRunStatus is crw manage audit round status.
func auditRoundRunStatus(_ context.Context, e *Env, args []string) int {
	values, err := auditPkgParseArgs(args, map[string]bool{"name": true})
	if err != nil || values["name"] == "" {
		if err == nil {
			err = errors.New("--name is required")
		}
		fmt.Fprintln(e.Stderr, auditRoundUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit round status: error: %v\n", err)
		return usageExit
	}
	cfg := coreDefaults(e)
	path, err := auditRoundPath(e, cfg, values["name"])
	if err != nil {
		fmt.Fprintln(e.Stderr, auditRoundUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit round status: error: %v\n", err)
		return usageExit
	}
	doc, err := auditRoundLoad(path)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit round status: error: %v\n", err)
		return 1
	}
	return auditRoundWriteStatus(e, doc)
}
