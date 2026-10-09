package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// crw manage improve run turns one plan boundary into a whole improvement pass: it runs
// collect and then propose, and writes one roadmap document that ranks the candidates the
// pass found. The product only proposes: it never releases, merges or writes to Linear, and
// it starts no timer (the accepted decision leaves the trigger to a person or the management
// session).

// improveRoadmapUsage is what the run subcommand prints.
const improveRoadmapUsage = "usage: crw manage improve run --boundary <milestone|project> --ref KEY"

// improveRoadmapBoundaries is the set of plan boundaries a run may name.
var improveRoadmapBoundaries = map[string]bool{"milestone": true, "project": true}

// improveReasonRoadmapLocked is the named refusal of a second improvement pass over one boundary
// and ref: the pass lock is held, so this run reports it rather than proposing against a state
// another run is still changing.
const improveReasonRoadmapLocked = "improve_roadmap_locked"

// improveRoadmapLock takes the pass lock for one boundary and ref, so two runs of one pass cannot
// both decide the pass has not happened yet and each spend the configured cap. It is non-blocking,
// like the drafts lock: a second run is refused by name rather than waiting. The lock file itself
// is never removed, because another process may hold it. The drafts lock is taken inside this one,
// so the two are always taken in that order.
func improveRoadmapLock(dir, boundary, ref string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := crwconfig.JoinRoot(dir, improveRoadmapLockName(boundary, ref))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %s is held by another run", improveReasonRoadmapLocked, filepath.Base(lockPath))
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// improveRoadmapLockName is the pass lock's file name: a fixed-length digest of the boundary and
// ref, so a long but legal ref cannot push the name past the filesystem's name limit. The two
// fields are hashed as a JSON array, so no delimiter inside either can make two passes share a
// name.
func improveRoadmapLockName(boundary, ref string) string {
	parts, err := json.Marshal([]string{boundary, ref})
	if err != nil {
		parts = []byte(boundary + "\x00" + ref)
	}
	sum := sha256.Sum256(parts)
	return "roadmap-" + hex.EncodeToString(sum[:])[:auditDraftFingerprintChars] + ".lock"
}

// improveRoadmapStampFormat is the UTC stamp a roadmap file name carries. It keeps
// nanoseconds, so two runs of one ref in the same second do not overwrite each other.
const improveRoadmapStampFormat = "20060102T150405.000000000Z"

// improveRoadmapParseArgs reads --boundary and --ref.
func improveRoadmapParseArgs(args []string) (boundary, ref string, help bool, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			return "", "", true, nil
		case args[i] == "--boundary":
			if i+1 >= len(args) {
				return "", "", false, errors.New("the option --boundary needs a value")
			}
			i++
			boundary = args[i]
		case strings.HasPrefix(args[i], "--boundary="):
			boundary = strings.TrimPrefix(args[i], "--boundary=")
		case args[i] == "--ref":
			if i+1 >= len(args) {
				return "", "", false, errors.New("the option --ref needs a value")
			}
			i++
			ref = args[i]
		case strings.HasPrefix(args[i], "--ref="):
			ref = strings.TrimPrefix(args[i], "--ref=")
		default:
			return "", "", false, fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if boundary == "" {
		return "", "", false, errors.New("the option --boundary is required")
	}
	if !improveRoadmapBoundaries[boundary] {
		return "", "", false, fmt.Errorf("the boundary %q is not milestone or project", boundary)
	}
	if strings.TrimSpace(ref) == "" {
		return "", "", false, errors.New("the option --ref needs a value")
	}
	// The ref names one directory below the manage state directory, so it must be a single
	// plain path segment: a ref carrying a separator or a relative component could move the
	// bundle outside the managed state.
	if !improveRoadmapPlainRef(ref) {
		return "", "", false, fmt.Errorf("the ref %q must be a plain name without a path separator", ref)
	}
	return boundary, ref, false, nil
}

// improveRoadmapPlainRef reports whether a boundary ref is one plain path segment: a non-empty
// name that is neither "." nor "..", carries no path separator, and holds no control character. A
// control character (U+0000 to U+001F, U+007F) is refused because the roadmap document writes the
// ref verbatim and the header reader compares it line by line: an embedded newline would let one
// ref's document stand in for another's, suppressing a first run or spending the cap on a rerun.
func improveRoadmapPlainRef(ref string) bool {
	if ref == "" || ref == "." || ref == ".." {
		return false
	}
	if strings.ContainsRune(ref, '/') || strings.ContainsRune(ref, '\\') {
		return false
	}
	for _, r := range ref {
		if r <= 0x1F || r == 0x7F {
			return false
		}
	}
	return true
}

// improveRoadmapDir is where the improvement pass keeps its bundles and roadmap documents:
// below the state directory the configuration names, so it never writes beside the checkout.
func improveRoadmapDir(e *Env, cfg *Config) string {
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "improve")
}

// improveRoadmapBundlePath is the bundle one run keeps: a per-run file beside its roadmap, so
// a later run of the same ref never replaces the evidence an earlier roadmap cites. The stamp
// is the run's own, so the bundle and the roadmap of one run pair by name.
func improveRoadmapBundlePath(dir, ref, stamp string) string {
	return crwconfig.JoinRoot(dir, ref, "bundle-"+stamp+".json")
}

// improveRoadmapStamp is the UTC stamp a roadmap file name carries.
func improveRoadmapStamp(now time.Time) string {
	return now.UTC().Format(improveRoadmapStampFormat)
}

// improveRoadmapDocument is the roadmap markdown: what the pass covered, how many drafts it
// left, and one ranked row per candidate with its frequency, impact, cost and evidence.
func improveRoadmapDocument(boundary, ref, bundle string, report improveProposeReport) string {
	var b strings.Builder
	b.WriteString("# Improvement roadmap\n\n")
	b.WriteString("- boundary: " + boundary + "\n")
	b.WriteString("- ref: " + ref + "\n")
	b.WriteString("- evidence bundle: " + bundle + "\n")
	b.WriteString(fmt.Sprintf("- created drafts: %d\n", len(report.Created)))
	b.WriteString(fmt.Sprintf("- updated drafts: %d\n", len(report.Updated)))
	b.WriteString(fmt.Sprintf("- suppressed by the issue list: %d\n", len(report.Suppressed)))
	b.WriteString(fmt.Sprintf("- left for a later run: %d\n", report.Remaining))
	b.WriteString("\n## Candidates\n\n")
	if len(report.Candidates) == 0 {
		b.WriteString("(the pass found no candidate)\n")
		return b.String()
	}
	for i, candidate := range report.Candidates {
		fmt.Fprintf(&b, "### %d. %s\n\n", i+1, candidate.Title)
		b.WriteString(fmt.Sprintf("- key: %s\n", candidate.Key))
		b.WriteString(fmt.Sprintf("- kind: %s\n", candidate.Kind))
		b.WriteString(fmt.Sprintf("- frequency: %d\n", candidate.Count))
		b.WriteString(fmt.Sprintf("- impact: %s\n", improveRoadmapImpactName(candidate.Impact)))
		b.WriteString(fmt.Sprintf("- estimated cost: %d\n", candidate.Cost))
		b.WriteString("- projects:\n")
		for _, project := range candidate.Projects {
			b.WriteString(fmt.Sprintf("  - %s (%d)\n", project.Project, project.Count))
		}
		b.WriteString("- evidence:\n")
		if len(candidate.Evidence) == 0 {
			b.WriteString("  - (the bundle recorded no origin location)\n")
		} else {
			for _, location := range candidate.Evidence {
				b.WriteString("  - " + location + "\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// improveRoadmapImpactName is an impact rank as the roadmap writes it.
func improveRoadmapImpactName(impact int) string {
	switch impact {
	case improveProposeImpactHigh:
		return "high"
	case improveProposeImpactMedium:
		return "medium"
	}
	return "low"
}

// improveRoadmapWriteFile writes the roadmap atomically: a temporary file beside it, fsynced,
// then renamed over it, so a reader never sees a half-written document.
func improveRoadmapWriteFile(path string, data []byte) error {
	dir := rootDir(path)
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// improveRoadmapRun runs one plan-boundary pass: collect writes the bundle, propose turns it
// into drafts, and the roadmap document is written last, so a failure before it leaves no
// roadmap. It returns the roadmap path it wrote.
func improveRoadmapRun(ctx context.Context, e *Env, boundary, ref string) (string, error) {
	cfg := coreDefaults(e)
	dir := improveRoadmapDir(e, cfg)
	// The pass is held for the whole run, so the check for a previous roadmap, the collection, the
	// proposal and the roadmap write happen under one lock: two runs of one boundary and ref cannot
	// both find no roadmap and each spend the configured cap.
	release, err := improveRoadmapLock(dir, boundary, ref)
	if err != nil {
		return "", err
	}
	defer release()
	stamp := improveRoadmapStamp(e.Now())
	bundlePath := improveRoadmapBundlePath(dir, ref, stamp)
	if err := os.MkdirAll(rootDir(bundlePath), 0o700); err != nil {
		return "", err
	}
	// collect is called first, so the bundle it writes is the evidence the roadmap cites.
	if code := improveRunCollect(ctx, e, []string{"--out", bundlePath}); code != 0 {
		return "", fmt.Errorf("collect exited with status %d", code)
	}
	// A boundary and ref that already have a roadmap document are not proposed again: the drafts
	// that exist still grow their seen list, and the candidates the cap leaves are counted for a
	// later run rather than drafted now.
	ran, err := improveRoadmapAlreadyRan(dir, boundary, ref)
	if err != nil {
		return "", err
	}
	capOverride := -1
	if ran {
		capOverride = 0
	}
	report, err := improveProposeRunCapped(ctx, e, bundlePath, false, capOverride)
	if err != nil {
		return "", err
	}
	path := crwconfig.JoinRoot(dir, "roadmap-"+stamp+".md")
	if err := improveRoadmapWriteFile(path, []byte(improveRoadmapDocument(boundary, ref, bundlePath, report))); err != nil {
		return "", err
	}
	return path, nil
}

// improveRoadmapAlreadyRan reports whether the roadmap directory already holds a document for this
// boundary and ref. The document names both in its header, so a run of the same boundary and ref is
// recognised by the document it wrote, not by a file name a clock chose. A roadmap the directory
// holds but cannot read is an error rather than a silent "never ran": treating an unreadable
// document as absent would spend the cap on a pass that already happened.
func improveRoadmapAlreadyRan(dir, boundary, ref string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	boundaryLine, refLine := "- boundary: "+boundary, "- ref: "+ref
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "roadmap-") || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(crwconfig.JoinRoot(dir, name))
		if err != nil {
			return false, err
		}
		if improveRoadmapHeaderCovers(string(data), boundaryLine, refLine) {
			return true, nil
		}
	}
	return false, nil
}

// improveRoadmapHeaderCovers reports whether a roadmap document's header names this boundary and
// ref. Only the lines before the first "## " section are read: a candidate title, a reason or an
// evidence location is written into the body verbatim, so a body line that happens to spell these
// lines must not stand in for the header the run itself wrote.
func improveRoadmapHeaderCovers(body, boundaryLine, refLine string) bool {
	hasBoundary, hasRef := false, false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") {
			break
		}
		switch line {
		case boundaryLine:
			hasBoundary = true
		case refLine:
			hasRef = true
		}
	}
	return hasBoundary && hasRef
}

// improveRunRoadmap is crw manage improve run. It prints the roadmap path it wrote.
func improveRunRoadmap(ctx context.Context, e *Env, args []string) int {
	boundary, ref, help, err := improveRoadmapParseArgs(args)
	if help {
		fmt.Fprintln(e.Stdout, improveRoadmapUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, improveRoadmapUsage)
		fmt.Fprintf(e.Stderr, "crw manage improve run: error: %v\n", err)
		return usageExit
	}
	path, err := improveRoadmapRun(ctx, e, boundary, ref)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve run: error: %v\n", err)
		return 1
	}
	fmt.Fprintln(e.Stdout, path)
	return 0
}
