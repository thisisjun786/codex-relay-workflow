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

// reviewRoundArgsInside is the working directory cwd's real path and the real path of abs, links followed, relative to it, and
// whether abs lies in that directory. This is the departure from the oracle, by decision (a review finding of kind security): the
// oracle read a path as spelled, so a link inside the workspace made it hash a file outside. A path whose real path cannot be named
// is not inside. The file is then read by reviewRoundArgsReadBelow, so a path swapped after this check cannot lead the read out.
func reviewRoundArgsInside(cwd, abs string) (base, below string, ok bool) {
	base, err := reviewRoundArgsAbs("", cwd)
	if err == nil {
		base, err = filepath.EvalSymlinks(base)
	}
	physical, perr := filepath.EvalSymlinks(abs)
	if err != nil || perr != nil {
		return "", "", false
	}
	below, err = filepath.Rel(base, physical)
	return base, below, err == nil && filepath.IsLocal(below)
}

// reviewRoundArgsReadBelow reads below, relative to the directory base, through os.Root, which refuses any path that leads out of
// base, a link swapped in since reviewRoundArgsInside included, and a link given as an absolute path.
func reviewRoundArgsReadBelow(base, below string) ([]byte, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(below)
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
// directory that cannot be read. Unlike the oracle each entry must have a real path inside the working directory
// (reviewRoundArgsInside), one that does not reads as missing, and the read stays below that directory (reviewRoundArgsReadBelow).
// An entry is decoded as Node decodes argv, so a stored key is valid text.
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
		info, statErr := os.Lstat(abs)
		root, below, inside := reviewRoundArgsInside(cwd, abs)
		if statErr != nil || !info.Mode().IsRegular() || !inside {
			return nil, fmt.Sprintf("plan path %s is not a readable regular file", p), nil
		}
		key, _ := filepath.Rel(base, abs)
		if seen[key] {
			continue
		}
		seen[key] = true
		data, err := reviewRoundArgsReadBelow(root, below)
		if err != nil {
			return nil, "", err
		}
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
// (reviewRoundArgsInside), which the oracle hashed.
func Recomputed(cwd string, files []goalplan.PlanFileHash) []goalplan.PlanFileHash {
	out := make([]goalplan.PlanFileHash, len(files))
	for i, f := range files {
		out[i] = goalplan.PlanFileHash{Path: f.Path, Sha256: "missing"}
		abs, err := reviewRoundArgsAbs(cwd, f.Path)
		if err != nil {
			continue
		}
		if root, below, inside := reviewRoundArgsInside(cwd, abs); inside {
			if data, err := reviewRoundArgsReadBelow(root, below); err == nil {
				out[i].Sha256 = reviewRoundArgsSum(data)
			}
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
