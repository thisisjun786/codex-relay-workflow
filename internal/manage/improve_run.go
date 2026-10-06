package manage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// name that is neither "." nor ".." and carries no path separator.
func improveRoadmapPlainRef(ref string) bool {
	if ref == "" || ref == "." || ref == ".." {
		return false
	}
	return !strings.ContainsRune(ref, '/') && !strings.ContainsRune(ref, '\\')
}

// improveRoadmapDir is where the improvement pass keeps its bundles and roadmap documents:
// below the state directory the configuration names, so it never writes beside the checkout.
func improveRoadmapDir(e *Env, cfg *Config) string {
	return filepath.Join(auditStateDir(e, cfg), "improve")
}

// improveRoadmapBundlePath is the bundle one run keeps: a per-run file beside its roadmap, so
// a later run of the same ref never replaces the evidence an earlier roadmap cites. The stamp
// is the run's own, so the bundle and the roadmap of one run pair by name.
func improveRoadmapBundlePath(dir, ref, stamp string) string {
	return filepath.Join(dir, ref, "bundle-"+stamp+".json")
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
	stamp := improveRoadmapStamp(e.Now())
	bundlePath := improveRoadmapBundlePath(dir, ref, stamp)
	if err := os.MkdirAll(filepath.Dir(bundlePath), 0o700); err != nil {
		return "", err
	}
	// collect is called first, so the bundle it writes is the evidence the roadmap cites.
	if code := improveRunCollect(ctx, e, []string{"--out", bundlePath}); code != 0 {
		return "", fmt.Errorf("collect exited with status %d", code)
	}
	report, err := improveProposeRun(e, bundlePath, false)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "roadmap-"+stamp+".md")
	if err := improveRoadmapWriteFile(path, []byte(improveRoadmapDocument(boundary, ref, bundlePath, report))); err != nil {
		return "", err
	}
	return path, nil
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
