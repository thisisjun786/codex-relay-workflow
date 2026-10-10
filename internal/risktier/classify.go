package risktier

import (
	"path"
	"strings"
	"unicode/utf8"
)

// Tier is a risk tier. There are two: a change is low only when every path it touches is on the known
// low-risk allowlist, and everything else, including every path nobody listed, is high.
type Tier string

const (
	Low  Tier = "low"
	High Tier = "high"
)

func (t Tier) valid() bool { return t == Low || t == High }

// Max is the higher of two tiers; an unknown tier is read as high so that it can never lower a result.
func Max(a, b Tier) Tier {
	if a == Low && b == Low {
		return Low
	}
	return High
}

// Verdict is the tier of one path and the rule that decided it.
type Verdict struct {
	Tier Tier
	Rule string
}

// The rules a verdict names.
const (
	RuleDocsProse       = "docs_prose"
	RuleDocsProseTree   = "docs_prose_tree"
	RuleTestdataData    = "testdata_data"
	RuleTestdataTree    = "testdata_tree"
	RuleSharedContract  = "shared_contract"
	RuleExecutableText  = "executable_instruction"
	RuleCorpusOrFixture = "corpus_or_fixture"
	RuleHighTree        = "high_tree"
	RuleNotOnAllowlist  = "not_on_allowlist"
	RuleInvalidPath     = "invalid_path"
	RuleTypeChange      = "type_change"
)

// The allowlist is the whole of "low": documentation prose under docs/ (Markdown only), and plain data
// files under a testdata directory. A path counts as low only when no name on it marks the file as a
// corpus, a fixture, a golden or a security reader's input. The "sweeps and small edits outside high
// paths" of the decided answer cannot be told from a path, so they are not on it: such a change is high
// until its tree is added here, one reviewed line at a time.
var (
	// reservedDocs are name parts that take a docs file out of the allowlist.
	reservedDocs = []string{"golden", "fixture", "corpus", "oracle", "recorded", "security"}
	// reservedTestdata are name parts that take a testdata file out of the allowlist.
	reservedTestdata = []string{"golden", "fixture", "corpus", "oracle", "recorded", "security", "replay", "hook", "shell"}
	testdataExts     = map[string]bool{".json": true, ".jsonl": true, ".ndjson": true, ".txt": true, ".md": true, ".toml": true, ".yaml": true, ".yml": true, ".csv": true}
	// highTrees are the trees the decided answer names. Every path outside the allowlist is high anyway;
	// the list only lets the record say which kind of high path it is.
	highTrees = []string{"internal/relay", "internal/runtime", "internal/hookswitch", "internal/policystore"}
	// sharedTrees and sharedFiles are the contract surfaces: the scheduler's SharedSurface list, with all of contract/.
	sharedTrees = []string{"contract"}
	sharedFiles = []string{"internal/relay/argparse/specs.json"}
	// instructionFiles are root documents that instruct agents or state the policy.
	instructionFiles = []string{"AGENTS.md", "CLAUDE.md", "POLICY.md", "SECURITY.md"}
)

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

func withinAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if within(p, d) {
			return true
		}
	}
	return false
}

// validPath is a clean, relative, slash-separated path inside the repository with printable UTF-8.
func validPath(p string) bool {
	if p == "" || p == "." || !utf8.ValidString(p) || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return p != ".." && !strings.HasPrefix(p, "../")
}

func reserved(p string, names []string) bool {
	lower := strings.ToLower(p)
	for _, n := range names {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func hasSegment(segments []string, name string) bool {
	for _, s := range segments {
		if s == name {
			return true
		}
	}
	return false
}

// testdataGolden is a path with a testdata/golden pair of segments: the goldens of a package are a contract.
func testdataGolden(segments []string) bool {
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "testdata" && segments[i+1] == "golden" {
			return true
		}
	}
	return false
}

// highFloor is the high verdict for a path that no low rule may lift: a shared contract surface or text
// that instructs an agent. ok is false when neither applies.
func highFloor(p string, segments []string) (Verdict, bool) {
	switch {
	case withinAny(p, sharedTrees), isOneOf(p, sharedFiles), testdataGolden(segments):
		return Verdict{High, RuleSharedContract}, true
	case within(p, "plugins"), isOneOf(p, instructionFiles):
		return Verdict{High, RuleExecutableText}, true
	}
	return Verdict{}, false
}

func isOneOf(p string, list []string) bool {
	for _, s := range list {
		if p == s {
			return true
		}
	}
	return false
}

func defaultHigh(p string) Verdict {
	if withinAny(p, highTrees) {
		return Verdict{High, RuleHighTree}
	}
	return Verdict{High, RuleNotOnAllowlist}
}

// Classify is the tier of one repository-relative file path. A path that is not a clean path inside the
// repository is high, so is every path the allowlist does not name.
func Classify(p string) Verdict {
	if !validPath(p) {
		return Verdict{High, RuleInvalidPath}
	}
	segments := strings.Split(p, "/")
	if v, ok := highFloor(p, segments); ok {
		return v
	}
	ext := path.Ext(p)
	switch {
	case segments[0] == "docs" && len(segments) > 1 && ext == ".md":
		if reserved(p, reservedDocs) {
			return Verdict{High, RuleCorpusOrFixture}
		}
		return Verdict{Low, RuleDocsProse}
	case hasSegment(segments[:len(segments)-1], "testdata") && testdataExts[ext]:
		if reserved(p, reservedTestdata) {
			return Verdict{High, RuleCorpusOrFixture}
		}
		return Verdict{Low, RuleTestdataData}
	}
	return defaultHigh(p)
}

// classifyTree is the tier of a declared tree region. A tree is low only when it lies in the docs prose
// area or in a testdata directory; what is really edited under it is judged from the changed paths.
func classifyTree(p string) Verdict {
	if !validPath(p) {
		return Verdict{High, RuleInvalidPath}
	}
	segments := strings.Split(p, "/")
	if v, ok := highFloor(p, segments); ok {
		return v
	}
	switch {
	case within(p, "docs"):
		if reserved(p, reservedDocs) {
			return Verdict{High, RuleCorpusOrFixture}
		}
		return Verdict{Low, RuleDocsProseTree}
	case hasSegment(segments, "testdata"):
		if reserved(p, reservedTestdata) {
			return Verdict{High, RuleCorpusOrFixture}
		}
		return Verdict{Low, RuleTestdataTree}
	}
	return defaultHigh(p)
}

// Region is one declared edit region (dag-region-declare): a tree, a file, or a symbol of a file.
type Region struct {
	Path   string
	Kind   string // tree, file or symbol
	Key    string // the symbol, for a symbol region only
	Change string // edit (the default), rename or delete
}

// ClassifyRegion is the tier of a declared region: a tree by its directory, a file or a symbol by its file.
func ClassifyRegion(r Region) Verdict {
	if r.Kind == "tree" {
		return classifyTree(r.Path)
	}
	return Classify(r.Path)
}

func (r Region) covers(p string) bool {
	if r.Kind == "tree" {
		return within(p, r.Path)
	}
	return p == r.Path
}
