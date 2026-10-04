package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ReviewRoundVerb is a review-round verb (review-round-cli.ts:30-31, CXC v0.2.40, 3c1459ac); ReviewRoundVerbHelp is what the parser
// answers for no verb or a help token.
type ReviewRoundVerb string

// The verbs of review-round.
const (
	ReviewRoundVerbOpen  ReviewRoundVerb = "open"
	ReviewRoundVerbShow  ReviewRoundVerb = "show"
	ReviewRoundVerbAbort ReviewRoundVerb = "abort"
	ReviewRoundVerbHelp  ReviewRoundVerb = "help"
)

// ReviewRoundCliArgs ports review-round-cli.ts:92-99. Session and Reason are nil where the oracle's field is undefined: never
// given, or a value flag that was the last token (which also undoes an earlier value of the same flag). An empty value is not nil.
type ReviewRoundCliArgs struct {
	Verb      ReviewRoundVerb `json:"verb"`
	Cwd       string          `json:"cwd"`
	Session   *string         `json:"session,omitempty"`
	PlanPaths []string        `json:"planPaths"`
	Reason    *string         `json:"reason,omitempty"`
	JSON      bool            `json:"json,omitempty"`
}

// ReviewRoundCliParsed holds exactly one of Args and a non-empty Error.
type ReviewRoundCliParsed struct {
	Args  *ReviewRoundCliArgs
	Error string
}

// ReviewRoundCliResult is review-round-cli.ts:128, the terminal answer of a run: the shape of CliResult.
type ReviewRoundCliResult = CliResult

// ParseReviewRoundCliArgs ports review-round-cli.ts:103-126. argv excludes the review-round token. The verb is case-insensitive;
// none, help, --help and -h answer help and ignore the rest. A value flag takes the next token even when it is a flag, unknown
// tokens are skipped, and an empty --plan-path is dropped.
func ParseReviewRoundCliArgs(argv []string, cwd string) ReviewRoundCliParsed {
	verb := ""
	if len(argv) > 0 {
		verb = strings.ToLower(argv[0])
	}
	if len(argv) == 0 || verb == "help" || verb == "--help" || verb == "-h" {
		return ReviewRoundCliParsed{Args: &ReviewRoundCliArgs{Verb: ReviewRoundVerbHelp, Cwd: cwd, PlanPaths: []string{}}}
	}
	switch ReviewRoundVerb(verb) {
	case ReviewRoundVerbOpen, ReviewRoundVerbShow, ReviewRoundVerbAbort:
	default:
		return ReviewRoundCliParsed{Error: fmt.Sprintf("unknown review-round verb '%s' (expected open|show|abort)", argv[0])}
	}
	out := &ReviewRoundCliArgs{Verb: ReviewRoundVerb(verb), Cwd: cwd, PlanPaths: []string{}}
	i := 0
	next := func() *string {
		if i++; i < len(argv) {
			v := argv[i]
			return &v
		}
		return nil
	}
	for i = 1; i < len(argv); i++ {
		switch argv[i] {
		case "--session":
			out.Session = next()
		case "--cwd":
			out.Cwd = cwd
			if v := next(); v != nil {
				out.Cwd = *v
			}
		case "--plan-path":
			if v := next(); v != nil && *v != "" {
				out.PlanPaths = append(out.PlanPaths, *v)
			}
		case "--reason":
			out.Reason = next()
		case "--json":
			out.JSON = true
		}
	}
	return ReviewRoundCliParsed{Args: out}
}

// RenderReviewRoundHelp ports review-round-cli.ts:172-190, with the verb named as the CLI name table renames it.
func RenderReviewRoundHelp() string {
	return strings.Join([]string{
		"crw pabcd review-round — the opt-in A-gate plan-audit round (LEAN-REVIEW-01)",
		"",
		"Usage:",
		"  crw pabcd review-round open --session <id> [--plan-path <path>]... [--cwd <path>] [--json]",
		"  crw pabcd review-round show --session <id> [--cwd <path>] [--json]",
		"  crw pabcd review-round abort --session <id> [--reason <text>] [--cwd <path>]",
		"  crw pabcd review-round --help",
		"",
		"Notes:",
		"  A round is OPTIONAL. With no round open, A>B advances on the attest alone.",
		"  With a round whose verdict was RECORDED, that verdict is binding: you cannot",
		"  attest \"pass\" over a reviewer's \"fail\".",
		"  open requires the session to be at phase A with a bound goalplan and a plan",
		"  binding recorded by P>A.",
		"  abort closes a round that no reviewer will finish, so the cycle is not stuck.",
	}, "\n")
}

func reviewRoundArgsSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PlanFilesHash ports review-round-cli.ts:168-170: the sha256 of "path NUL sha256" joined by NUL, in the order given. The oracle's
// comment calls this freeze.ts's construction; that one hashes the sha256 values alone, sorted (known-defects.md).
func PlanFilesHash(files []goalplan.PlanFileHash) string {
	parts := make([]string, len(files))
	for i, f := range files {
		parts[i] = f.Path + "\x00" + f.Sha256
	}
	return reviewRoundArgsSum([]byte(strings.Join(parts, "\x00")))
}

// reviewRoundArgsAbs is path.resolve(cwd, p): p when absolute, else cwd joined with it, and a result still relative is made absolute
// against the kernel's working directory (syscall.Getwd, which is what process.cwd() answers; os.Getwd would trust $PWD). The error
// is where Node throws: the working directory cannot be read.
func reviewRoundArgsAbs(cwd, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	wd, err := syscall.Getwd()
	return filepath.Join(wd, p), err
}

// reviewRoundArgsBase is the working directory opened once as an os.Root, so that every decision about an entry and every read is
// bound to that one directory handle. This is the departure from the oracle, by decision (a review finding of kind security): the
// oracle read a path as spelled, so a link inside the workspace made it hash a file outside. Here an entry is read only when its
// real path, links followed, lies in the working directory's real path, and the read is made through the handle, which refuses any
// path that leads out of the directory, a link swapped in after the check included; replacing the directory itself or one of its
// ancestors after the open leaves the read on the directory that was checked. real is the real path root was opened on.
type reviewRoundArgsBase struct {
	root *os.Root
	real string
}

// reviewRoundArgsOpenBase opens the working directory cwd. The result is nil, with no error, when it cannot be opened or is not the
// directory its real path names (replaced while it was opened); every entry then reads as outside. The error is where Node throws:
// a relative working directory that cannot be made absolute.
func reviewRoundArgsOpenBase(cwd string) (*reviewRoundArgsBase, error) {
	abs, err := reviewRoundArgsAbs("", cwd)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, nil
	}
	real, err := filepath.EvalSymlinks(abs)
	opened, openedErr := root.Stat(".")
	var named os.FileInfo
	if err == nil {
		named, err = os.Stat(real)
	}
	if err != nil || openedErr != nil || !os.SameFile(opened, named) {
		root.Close()
		return nil, nil
	}
	return &reviewRoundArgsBase{root, real}, nil
}

func (b *reviewRoundArgsBase) close() {
	if b != nil {
		b.root.Close()
	}
}

// inside is the real path of abs relative to the base's real path, and whether abs lies in it. A path whose real path cannot be
// named is not inside.
func (b *reviewRoundArgsBase) inside(abs string) (below string, ok bool) {
	if b == nil {
		return "", false
	}
	physical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	below, err = filepath.Rel(b.real, physical)
	return below, err == nil && filepath.IsLocal(below)
}

// read reads below, relative to the base, through the handle.
func (b *reviewRoundArgsBase) read(below string) ([]byte, error) { return b.root.ReadFile(below) }

// readInside is inside followed by read: the file abs names, its real path relative to the base, whether it is inside (nothing is
// read when it is not) and the error of the read.
func (b *reviewRoundArgsBase) readInside(abs string) (data []byte, below string, inside bool, err error) {
	if below, inside = b.inside(abs); inside {
		data, err = b.read(below)
	}
	return data, below, inside, err
}

// reviewRoundArgsNumberedDoc is NUMBERED_DOC_RE, /^\d{3}_.+\.md$/ (review-round-cli.ts:34): three ASCII digits, an underscore, at
// least one character that is not a JavaScript line terminator, and ".md". A subdirectory document such as 000_a/b.md matches.
func reviewRoundArgsNumberedDoc(rel string) bool {
	stem, ok := strings.CutSuffix(rel, ".md")
	if !ok || len(stem) < 5 || stem[3] != '_' || strings.ContainsAny(stem[4:], "\n\r\u2028\u2029") {
		return false
	}
	return strings.Trim(stem[:3], "0123456789") == ""
}

// reviewRoundArgsCollectPlanFiles ports collectPlanFiles (review-round-cli.ts:141-165): the --plan-path entries resolved against
// the unit P>A validated. refusal is the message of a refused entry; err is where the oracle throws, an unreadable file or a working
// directory that cannot be read. Unlike the oracle each entry must have a real path inside the working directory and is read
// through reviewRoundArgsBase; one that does not reads as missing. An entry is decoded as Node decodes argv, so a stored key is
// valid text.
func reviewRoundArgsCollectPlanFiles(cwd, planUnit string, paths []string) ([]goalplan.PlanFileHash, string, error) {
	if len(paths) == 0 {
		return nil, "--plan-path is required at least once: a round with no files audits nothing", nil
	}
	unit, err := reviewRoundArgsAbs(cwd, planUnit)
	if err != nil {
		return nil, "", err
	}
	base, err := reviewRoundArgsAbs("", cwd)
	if err != nil {
		return nil, "", err
	}
	opened, err := reviewRoundArgsOpenBase(cwd)
	if err != nil {
		return nil, "", err
	}
	defer opened.close()
	var files []goalplan.PlanFileHash
	seen := map[string]bool{}
	for _, p := range paths {
		p = source.DecodeUTF8([]byte(p))
		abs, err := reviewRoundArgsAbs(cwd, p)
		if err != nil {
			return nil, "", err
		}
		rel, _ := filepath.Rel(unit, abs)
		if strings.HasPrefix(rel, "..") {
			return nil, fmt.Sprintf("plan path %s is outside the bound plan unit %s", p, planUnit), nil
		}
		if !reviewRoundArgsNumberedDoc(rel) {
			return nil, fmt.Sprintf("plan path %s is not a numbered plan document (000_*.md) directly inside %s", p, planUnit), nil
		}
		unreadable := fmt.Sprintf("plan path %s is not a readable regular file", p)
		if info, err := os.Lstat(abs); err != nil || !info.Mode().IsRegular() {
			return nil, unreadable, nil
		}
		data, below, inside, err := opened.readInside(abs)
		if !inside {
			return nil, unreadable, nil
		}
		if err != nil {
			return nil, "", err
		}
		key, _ := filepath.Rel(base, abs)
		if !filepath.IsLocal(key) {
			// The key as the oracle stores it climbs out ("../ws/...") and the revival of a round's plan files drops a list holding
			// such a key: the entry is spelled by the real path of a working directory that is a link, or by another alias of it.
			// The first fallback keeps the links of the entry's own spelling, which the binding of the round depends on.
			if key, _ = filepath.Rel(opened.real, abs); !filepath.IsLocal(key) {
				key = below
			}
		}
		if !utf8.ValidString(key) {
			return nil, unreadable, nil
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		files = append(files, goalplan.PlanFileHash{Path: key, Sha256: reviewRoundArgsSum(data)})
	}
	// JavaScript compares strings by UTF-16 code unit, which puts a supplementary character before U+E000; byte order does not.
	slices.SortFunc(files, func(a, b goalplan.PlanFileHash) int {
		return slices.Compare(utf16.Encode([]rune(a.Path)), utf16.Encode([]rune(b.Path)))
	})
	return files, "", nil
}

// Recomputed ports review-round-cli.ts:308-317: the files a round named, read again, each entry keeping its path and its place.
// An unreadable file reads "missing", and so does an entry whose real path is outside the working directory
// (reviewRoundArgsBase), which the oracle hashed.
func Recomputed(cwd string, files []goalplan.PlanFileHash) []goalplan.PlanFileHash {
	out := make([]goalplan.PlanFileHash, len(files))
	opened, err := reviewRoundArgsOpenBase(cwd)
	if err != nil {
		opened = nil
	}
	defer opened.close()
	for i, f := range files {
		out[i] = goalplan.PlanFileHash{Path: f.Path, Sha256: "missing"}
		abs, err := reviewRoundArgsAbs(cwd, f.Path)
		if err != nil {
			continue
		}
		if data, _, inside, err := opened.readInside(abs); inside && err == nil {
			out[i].Sha256 = reviewRoundArgsSum(data)
		}
	}
	return out
}

// JavaScript's \s and dot, which differ from Go's: \s also holds \v, U+00A0, U+FEFF and the other Unicode spaces, and the dot
// excludes the four line terminators. internal/pabcd/hook/lint.go has the same classes and does not export them.
const (
	reviewRoundArgsJSSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	reviewRoundArgsJSDot   = `[^\n\r\x{2028}\x{2029}]`
)

// reviewRoundArgsTomlTableBody ports tomlTableBody (review-round-cli.ts:37-46): the lines after the first "[header]" line, up to
// the next line that opens a table. The split is on LF and CRLF, so a line can keep a lone CR, which \s and the dot treat as a
// terminator.
func reviewRoundArgsTomlTableBody(content, header string) (string, bool) {
	lines := text.SplitLines(content)
	opens := regexp.MustCompile(`^` + reviewRoundArgsJSSpace + `*\[`)
	headed := regexp.MustCompile(`^` + reviewRoundArgsJSSpace + `*\[` + regexp.QuoteMeta(header) + `\]` + reviewRoundArgsJSSpace + `*(?:#` + reviewRoundArgsJSDot + `*)?$`)
	start := slices.IndexFunc(lines, headed.MatchString)
	if start == -1 {
		return "", false
	}
	rest := lines[start+1:]
	if end := slices.IndexFunc(rest, opens.MatchString); end != -1 {
		rest = rest[:end]
	}
	return strings.Join(rest, "\n"), true
}

// reviewRoundArgsJSLines turns every line terminator JavaScript's m flag knows into LF, the one Go's (?m) knows.
func reviewRoundArgsJSLines(body string) string {
	return strings.NewReplacer("\r", "\n", "\u2028", "\n", "\u2029", "\n").Replace(body)
}

// reviewRoundArgsTomlBool ports tomlBoolInBody (review-round-cli.ts:48-51): the first "key = true|false" line, a comment allowed,
// and whether there was one.
func reviewRoundArgsTomlBool(body, key string) (value, found bool) {
	match := regexp.MustCompile(`(?m)^` + reviewRoundArgsJSSpace + `*` + regexp.QuoteMeta(key) + reviewRoundArgsJSSpace + `*=` + reviewRoundArgsJSSpace +
		`*(true|false)` + reviewRoundArgsJSSpace + `*(?:#` + reviewRoundArgsJSDot + `*)?$`).FindStringSubmatch(reviewRoundArgsJSLines(body))
	return match != nil && match[1] == "true", match != nil
}

// reviewRoundArgsV2Config is the reading half of v2SpawnSurface (review-round-cli.ts:76-89): the feature is on as a table, as a
// scalar under [features], or as an inline table there, in that order of precedence.
func reviewRoundArgsV2Config(content string) bool {
	if table, ok := reviewRoundArgsTomlTableBody(content, "features.multi_agent_v2"); ok {
		enabled, _ := reviewRoundArgsTomlBool(table, "enabled")
		return enabled
	}
	features, ok := reviewRoundArgsTomlTableBody(content, "features")
	if !ok {
		return false
	}
	if value, found := reviewRoundArgsTomlBool(features, "multi_agent_v2"); found {
		return value
	}
	inline := regexp.MustCompile(`(?m)^` + reviewRoundArgsJSSpace + `*multi_agent_v2` + reviewRoundArgsJSSpace + `*=` + reviewRoundArgsJSSpace +
		`*\{([^}]*)\}`).FindStringSubmatch(reviewRoundArgsJSLines(features))
	if inline == nil {
		return false
	}
	enabled := regexp.MustCompile(`enabled` + reviewRoundArgsJSSpace + `*=` + reviewRoundArgsJSSpace + `*(true|false)`).FindStringSubmatch(inline[1])
	return enabled != nil && enabled[1] == "true"
}

// reviewRoundArgsV2SpawnSurface ports v2SpawnSurface (review-round-cli.ts:67-90): whether the multi_agent_v2 spawn surface is on in
// the Codex config, <CODEX_HOME>/config.toml when CODEX_HOME is set and not empty (not trimmed), else ~/.codex/config.toml. A missing
// or unreadable config reads false; a home that cannot be read or made absolute is an error, where the oracle's homedir() and
// resolve() throw outside its try. A nil env is os.LookupEnv.
func reviewRoundArgsV2SpawnSurface(env host.LookupEnv) (bool, error) {
	if env == nil {
		env = os.LookupEnv
	}
	home, _ := env("CODEX_HOME")
	if home == "" {
		user, err := host.Home(env)
		if err == nil {
			home, err = reviewRoundArgsAbs(user, ".codex")
		}
		if err != nil {
			return false, err
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	return err == nil && reviewRoundArgsV2Config(string(data)), nil
}

// The two dispatch sentences of the open packet. The oracle picks one by v2SpawnSurface() and the two are the same text, so the
// config never changes the packet (known-defects.md); the choice is kept as the oracle makes it.
const (
	reviewRoundArgsDispatchV2 = `Dispatch an independent reviewer (agent_type "reviewer" if exposed; otherwise agent_type "explorer" with CRW-ROLE: reviewer before TASK:; if the host has no agent_type field, omit agent_type and prepend CRW-ROLE: reviewer before TASK:) and require it to end its`
	reviewRoundArgsDispatchV1 = `Dispatch an independent reviewer (agent_type "reviewer" if exposed; otherwise agent_type "explorer" with CRW-ROLE: reviewer before TASK:; if the host has no agent_type field, omit agent_type and prepend CRW-ROLE: reviewer before TASK:) and require it to end its`
)

// reviewRoundArgsRenderOpenPacket ports renderOpenPacket (review-round-cli.ts:192-208): what the agent that opened a round is told
// to do next, with the role header as rule R9 of the name table renames it. The error is that of reviewRoundArgsV2SpawnSurface.
func reviewRoundArgsRenderOpenPacket(round goalplan.ReviewRoundState, fileCount int, env host.LookupEnv) (string, error) {
	v2, err := reviewRoundArgsV2SpawnSurface(env)
	if err != nil {
		return "", err
	}
	launchID, dispatch := round.Lane.LaunchID, reviewRoundArgsDispatchV1
	if v2 {
		dispatch = reviewRoundArgsDispatchV2
	}
	return strings.Join([]string{
		launchID,
		"",
		fmt.Sprintf("Round %s is in flight over %d file(s).", round.RoundID, fileCount),
		dispatch,
		"final message with exactly these two lines:",
		"",
		"  LAUNCH: " + launchID,
		"  VERDICT: PASS | NEAR-PASS | FAIL",
		"",
		"The verdict is recorded when that reviewer exits. There is no way to write it here.",
	}, "\n"), nil
}
