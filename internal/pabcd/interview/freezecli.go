package interview

// The freeze command of CXC v0.2.40 (pabcd-state freeze-cli.ts and the freeze branch of cli.ts, commit 3c1459ac). Unlike the rest of
// the package this file reads and writes files: the plan under .crw/plan/<slug>/ and .crw/interview/freeze.json.

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/jsnode"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const freezeHelp = "crw work freeze \u2014 build or preview the interview freeze manifest\n\nUsage:\n" +
	"  crw work freeze --session <id> [--cwd <path>]\n" +
	"  crw work freeze --dry-run --session <id> [--cwd <path>]\n" +
	"  crw work freeze --help\n\nNotes:\n" +
	"  Hashes the plan files under .crw/plan/<slug>/ and writes the manifest\n" +
	"  at .crw/interview/freeze.json, then reports staleness against any\n" +
	"  existing manifest.\n" +
	"  --dry-run previews without writing. --help never writes."

// SessionReader is the session read of the command, state.ReadState's slug and interview tracker (this package cannot import state:
// state imports it); an unreadable session reads as an empty slug and a nil tracker.
type SessionReader func(cwd, sessionID string) (slug string, tracker *Tracker)

// FreezeCliArgs are the arguments of crw pabcd freeze.
type FreezeCliArgs struct {
	Cwd       string
	SessionID string
	DryRun    bool
	Help      bool
}

// ParseFreezeArgs takes the element after a flag's first occurrence as its value, even another flag; help is an empty argv or any element
// help, --help or -h. Without --cwd the kernel's working directory is read here, so help fails in a deleted directory as under Node.
func ParseFreezeArgs(argv []string) (FreezeCliArgs, error) {
	value := func(flag string) (string, bool) {
		if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
			return argv[i+1], true
		}
		return "", false
	}
	args := FreezeCliArgs{SessionID: "default", DryRun: slices.Contains(argv, "--dry-run")}
	args.Help = len(argv) == 0 || slices.ContainsFunc(argv, func(a string) bool { return a == "help" || a == "--help" || a == "-h" })
	if cwd, ok := value("--cwd"); ok {
		args.Cwd = cwd
	} else if wd, err := syscall.Getwd(); err == nil {
		args.Cwd = wd
	} else {
		return args, err
	}
	if id, ok := value("--session"); ok {
		args.SessionID = id
	}
	return args, nil
}

// ListPlanFiles hashes every file below planDir except the dot-names, with paths relative to planDir. It is existsSync, statSync and
// readdirSync: a directory that cannot be stat'ed is no files, one that can and cannot be read is an error, links are followed, and a
// name is decoded as Node decodes it (invalid bytes become U+FFFD) before it is joined, stat'ed, read and reported. A file is hashed
// as the text readFileSync(path, "utf8") returns, so its invalid bytes hash as U+FFFD. Unlike the oracle it refuses a plan directory or
// a file whose real path is outside the working directory cwd (see confined).
func ListPlanFiles(cwd, planDir string) ([]PlanFileHash, error) {
	if _, err := os.Stat(planDir); err != nil {
		return nil, nil
	}
	if err := confined(cwd, planDir); err != nil {
		return nil, err
	}
	var files []PlanFileHash
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := jsnode.DecodeUTF8([]byte(entry.Name()))
			if strings.HasPrefix(name, ".") {
				continue
			}
			full, shown := filepath.Join(dir, name), path.Join(rel, name)
			info, err := os.Stat(full)
			if err == nil {
				err = confined(cwd, full)
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				err = walk(full, shown)
			} else if data, readErr := os.ReadFile(full); readErr != nil {
				err = readErr
			} else {
				files = append(files, PlanFileHash{Path: shown, Sha256: Sha256(jsnode.DecodeUTF8(data))})
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	return files, walk(planDir, "")
}

// confined fails unless p, with its links resolved, is the working directory cwd or lies below it. Both are made absolute first, a relative
// one against syscall.Getwd, the kernel's directory (os.Getwd prefers $PWD), and cleaned as filepath.Join cleaned the work paths, so the
// path judged is the path opened: resolving links first would read "<link>/.." as the parent of the link's target.
func confined(cwd, p string) error {
	var real [2]string
	for i, q := range [2]string{cwd, p} {
		if !filepath.IsAbs(q) {
			wd, err := syscall.Getwd()
			if err != nil {
				return err
			}
			q = filepath.Join(wd, q)
		}
		r, err := filepath.EvalSymlinks(filepath.Clean(q))
		if err != nil {
			return err
		}
		real[i] = r
	}
	if rel, err := filepath.Rel(real[0], real[1]); err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s resolves outside the working directory", p)
	}
	return nil
}

// checkManifestPlace runs before the manifest is published. Lstat does not follow a link, so a link or a file in place of .crw or
// .crw/interview is refused, and so is a manifest that is a link, which crwdir.Publish would follow: the oracle writes through all three.
func checkManifestPlace(cwd, stateDir, manifestPath string) error {
	dir, plain := filepath.Dir(manifestPath), func(p string) error {
		info, err := os.Lstat(p)
		if err == nil && !info.IsDir() {
			err = fmt.Errorf("%s is a link or not a directory", p)
		}
		return err
	}
	err := plain(stateDir)
	if err == nil {
		err = os.MkdirAll(dir, 0o777)
	}
	if err == nil {
		err = errors.Join(plain(dir), confined(cwd, dir))
	}
	if info, lerr := os.Lstat(manifestPath); err == nil && lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		err = fmt.Errorf("%s is a symbolic link", manifestPath)
	}
	return err
}

// jsOf is obj[key] as a jsVal; id tells the objects and arrays of one manifest apart.
func jsOf(obj map[string]any, key string, id int) jsVal {
	v, present := obj[key]
	switch x := v.(type) {
	case nil:
		if present {
			return jsVal{kind: 'z'}
		}
		return jsVal{kind: 'u'}
	case string:
		return jsString(x)
	case bool:
		return jsVal{kind: 'b', s: fmt.Sprint(x)}
	case json.Number: // beyond float64's range ParseFloat answers an infinity, as JSON.parse does
		n, _ := strconv.ParseFloat(string(x), 64)
		if n == 0 {
			n = 0 // -0 and 0 are one Map key
		}
		return jsVal{kind: 'n', n: n}
	}
	return jsVal{kind: 'o', id: id, bad: throwsToString(v)}
}

func throwsToString(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		_, own := x["toString"]
		return own
	case []any:
		return slices.ContainsFunc(x, throwsToString)
	}
	return false
}

// readPrior reads the earlier manifest as checkStale meets it. ok is false where the oracle's try block fails: the file cannot be read
// or parsed, the root is not an object, planFiles is not an array, or an entry is null. Every other value is kept as JavaScript holds
// it, through the repository's lossless reader: a lone surrogate escape stays the WTF-8 bytes a Go string holds it in (Surrogates), so
// a manifest naming the escape of U+D800 is a different path than the file U+FFFD.md the plan really holds, as it is in JavaScript.
// Numbers stay as spelled, so a number past float64's range still reads as an infinity, and an object or array reads as the map and
// slice jsOf takes. jsnode.DecodeUTF8 comes first, as readFileSync does: it replaces each ill-formed subpart with one U+FFFD, where the
// reader would replace each byte.
func readPrior(file string) (frozen []frozenEntry, planHash jsVal, ok bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, jsVal{}, false
	}
	doc, err := pyjson.Loads(jsnode.DecodeUTF8(raw), pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Surrogates: true})
	if err != nil {
		return nil, jsVal{}, false
	}
	root, isObject := doc.(map[string]any)
	if !isObject {
		return nil, jsVal{}, false
	}
	entries, isArray := root["planFiles"].([]any)
	if !isArray {
		return nil, jsVal{}, false
	}
	for i, entry := range entries {
		if entry == nil {
			return nil, jsVal{}, false
		}
		obj, _ := entry.(map[string]any)
		frozen = append(frozen, frozenEntry{jsOf(obj, "path", i), jsOf(obj, "sha256", i)})
	}
	return frozen, jsOf(root, "planHash", -1), true
}

// RunFreeze hashes the plan under .crw/plan/<slug>/, reports the earlier manifest's staleness and, unless it is a dry run, publishes the
// new manifest atomically (crwdir.Publish: the oracle rewrites the file in place, so a crash could leave it truncated). The slug is the
// session's, else the session id. It returns the summary the command prints.
func RunFreeze(args FreezeCliArgs, read SessionReader) (string, error) {
	if args.Help {
		return freezeHelp, nil
	}
	slug, tracker := read(args.Cwd, args.SessionID)
	objective := cmp.Or(slug, args.SessionID)
	derived, stateDir := DeriveSlug(objective), filepath.Join(args.Cwd, crwdir.DirName)
	files, err := ListPlanFiles(args.Cwd, filepath.Join(stateDir, PlanSubdir, derived))
	if err != nil {
		return "", err
	}
	bundle := EvidenceBundle{OpenAssumptions: []string{}, Contradictions: []Contradiction{}, AcceptanceCriteria: []string{}}
	if tracker != nil {
		bundle.Dimensions = &tracker.Dimensions
		if tracker.Contradictions != nil {
			bundle.Contradictions = tracker.Contradictions
		}
		for _, a := range tracker.Assumptions {
			if a.Recorded {
				bundle.OpenAssumptions = append(bundle.OpenAssumptions, "- "+a.Text)
			}
		}
	}
	manifest := BuildFreezeManifest(BuildManifestInput{Objective: objective, PlanFiles: files, EvidenceBundle: bundle})
	manifestPath := filepath.Join(stateDir, FreezeManifestDir, FreezeManifestFile)

	staleLine := "stale-check: no prior manifest"
	if _, err := os.Stat(manifestPath); err == nil {
		staleLine = "stale-check: prior manifest unreadable (will re-freeze)"
		if frozen, hash, ok := readPrior(manifestPath); ok {
			if r, err := staleCheck(frozen, hash, files); err == nil && r.Stale {
				staleLine = "stale-check: STALE \u2014 " + r.Reason
			} else if err == nil {
				staleLine = "stale-check: fresh \u2014 " + r.Reason
			}
		}
	}
	if !args.DryRun {
		data, err := jsnode.Stringify(manifest, "  ")
		if err == nil {
			_, err = crwdir.EnsureDir(args.Cwd)
		}
		if err == nil {
			err = checkManifestPlace(args.Cwd, stateDir, manifestPath)
		}
		if err == nil {
			err = crwdir.Publish(manifestPath, data)
		}
		if err != nil {
			return "", err
		}
	}
	ready := IsInterviewReady(tracker)
	title := "[crw freeze]"
	if args.DryRun {
		title = "[crw freeze --dry-run]"
	}
	lines := []string{
		title,
		"manifest: " + manifestPath,
		"slug: " + derived,
		fmt.Sprintf("planFiles: %d", len(files)),
		"planHash: " + manifest.PlanHash,
		fmt.Sprintf("interviewReady: %t", ready),
		fmt.Sprintf("openAssumptions: %d", len(bundle.OpenAssumptions)),
		staleLine,
	}
	if ready { // freeze tells the main session to call create_goal; crw never writes the host's goal database
		lines = append(lines, "", GoalActivationDirective)
	}
	return strings.Join(lines, "\n"), nil
}

// FreezeCommand is crw pabcd freeze: the summary and a newline on stdout and exit 0, or "freeze failed: <error>" on stderr and exit 1.
func FreezeCommand(argv []string, stdout, stderr io.Writer, read SessionReader) int {
	args, err := ParseFreezeArgs(argv)
	out := ""
	if err == nil {
		out, err = RunFreeze(args, read)
	}
	if err != nil {
		fmt.Fprintf(stderr, "freeze failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, out)
	return 0
}
