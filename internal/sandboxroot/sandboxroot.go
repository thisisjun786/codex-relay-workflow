// Package sandboxroot is the one rule that decides where a sandboxed Codex may run (CRW-1265,
// decision C). The Codex linux sandbox (bubblewrap) makes and removes an empty .git mount target in
// a working root that has no .git of its own, so a sandbox whose working root (or writable root) is
// the account home, or a directory above other projects, makes <home>/.git appear and vanish and
// every tool that walks the home reads the home as a git repository in that instant. The rule keeps
// a sandboxed Codex in a dedicated root: the crwconfig sandbox_root (its default is crwconfig.DefaultSandboxRoot), a known
// project root or a linked worktree, and never the account home, a directory above it, or a
// directory above a known project root.
//
// Every product path that starts or resumes a sandboxed Codex, or hands a cwd to the App Server,
// calls Check (the working root), CheckWritable (a writable root) or CheckDestination (a worktree
// that does not exist yet) before it calls Codex or the App Server. No path has a check of its own.
// The coordinator's Python guard implements the same rule and runs the same table
// (testdata/cases.json, described in testdata/README.md).
package sandboxroot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// Code is the error code of every refusal.
const Code = "sandbox_root_refused"

// The rule names a Refusal carries, in the order Check applies them.
const (
	RuleRelative             = "relative"
	RuleMissing              = "missing"
	RuleUnresolvable         = "unresolvable"
	RuleFilesystemRoot       = "filesystem_root"
	RuleAccountHome          = "account_home"
	RuleHomeAncestor         = "home_ancestor"
	RuleProjectAncestor      = "project_ancestor"
	RuleOutsideDedicatedRoot = "outside_dedicated_root"
	RuleSandboxRootUnusable  = "sandbox_root_unusable"
)

// The sandbox spellings the rule distinguishes. Any other value, "" aside, counts as a sandbox that
// is on: a mode that cannot be told is treated as enabled.
const (
	// DangerFullAccess is the mode that creates no sandbox (and Bypass, the flag that skips it).
	DangerFullAccess = "danger-full-access"
	Bypass           = "bypass"
	// Unknown is a sandbox the caller could not determine. It is on and does not consult config.toml.
	Unknown = "unknown"
)

// Refusal is a working root, writable root or destination the rule turned away.
type Refusal struct {
	Path     string   // the candidate as received
	Resolved string   // the real path the verdict was made on (empty when none could be made)
	Rule     string   // the first rule that applied
	Project  string   // the project root a project_ancestor candidate is above
	Allowed  []string // the sandbox base path and the worktree base paths
	Hint     string   // what to change, for sandbox_root_unusable
}

// Code is the refusal's error code.
func (r *Refusal) Code() string { return Code }

func (r *Refusal) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %q is refused as a sandbox Codex root (rule %s", Code, r.Path, r.Rule)
	if r.Resolved != "" && r.Resolved != r.Path {
		fmt.Fprintf(&b, ", resolved to %s", r.Resolved)
	}
	b.WriteString(")")
	switch r.Rule {
	case RuleAccountHome, RuleHomeAncestor:
		b.WriteString(": the account home and the directories above it cannot be a sandbox root, because the Codex sandbox makes and removes an empty .git there")
	case RuleProjectAncestor:
		fmt.Fprintf(&b, ": it is above the project root %s", r.Project)
	case RuleOutsideDedicatedRoot:
		b.WriteString(": it is outside the dedicated roots")
	case RuleSandboxRootUnusable:
		b.WriteString(": the sandbox base path is unusable")
	}
	if r.Hint != "" {
		b.WriteString("; " + r.Hint)
	}
	if len(r.Allowed) > 0 {
		b.WriteString("; allowed bases: " + strings.Join(r.Allowed, ", "))
	}
	return b.String()
}

// environment is what the rule reads from the process: the variables and the account home.
var environment struct {
	sync.Mutex
	getenv      func(string) string
	accountHome string
	injected    bool
}

// Inject replaces the process facts the rule reads, until the returned restore runs: getenv stands
// for the environment (HOME, CODEX_HOME, CRW_CONFIG, XDG_*) and accountHome for the passwd home. A
// test uses it so that the real home, the real ~/.codex and the real crw configuration never
// decide a verdict; a nil getenv reads the process environment.
func Inject(getenv func(string) string, accountHome string) (restore func()) {
	environment.Lock()
	prevGetenv, prevHome, prevInjected := environment.getenv, environment.accountHome, environment.injected
	environment.getenv, environment.accountHome, environment.injected = getenv, accountHome, true
	environment.Unlock()
	return func() {
		environment.Lock()
		environment.getenv, environment.accountHome, environment.injected = prevGetenv, prevHome, prevInjected
		environment.Unlock()
	}
}

func facts() (func(string) string, string) {
	environment.Lock()
	defer environment.Unlock()
	getenv := environment.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if environment.injected {
		return getenv, environment.accountHome
	}
	return getenv, passwdHome()
}

// passwdHome is the home the passwd database gives the process's user, never $HOME. It is empty when
// the database names none.
func passwdHome() string {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || account.HomeDir == "" {
		return ""
	}
	return account.HomeDir
}

// rules is the resolved input of one verdict.
type rules struct {
	sandboxBase  string   // real path of sandbox_root, empty when unusable
	unusable     *Refusal // set when the sandbox base cannot be used
	worktreeBase []string // real paths of worktree_root and $CODEX_HOME/worktrees (the first may not exist)
	homes        []string // real paths of the protected homes
	projects     []string // sorted real paths of the known project roots
	rawBases     []string // the bases as configured, for the refusal text
	codexDoc     map[string]any
}

// best resolves a path to its real path, or cleans it when it does not exist.
func best(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// within reports whether p is dir or lies below it. Both are clean absolute paths.
func within(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// below reports whether p lies strictly below dir.
func below(p, dir string) bool { return p != dir && within(p, dir) }

func isHomeOrAbove(p string, homes []string) (rule string) {
	for _, home := range homes {
		if p == home {
			return RuleAccountHome
		}
	}
	for _, home := range homes {
		if below(home, p) {
			return RuleHomeAncestor
		}
	}
	return ""
}

func hasGit(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

func codexHome(getenv func(string) string, accountHome string) string {
	if value := getenv("CODEX_HOME"); value != "" {
		return value
	}
	home := getenv("HOME")
	if home == "" {
		home = accountHome
	}
	if home == "" {
		return ""
	}
	return crwconfig.JoinRoot(home, ".codex")
}

func load() *rules {
	getenv, accountHome := facts()
	r := &rules{}
	file, err := crwconfig.Load(getenv, "")
	if err != nil {
		r.unusable = &Refusal{Path: "", Rule: RuleSandboxRootUnusable, Hint: "the crw configuration cannot be read: " + err.Error()}
		return r
	}
	roots := file.Roots()
	configured := roots[crwconfig.RootSandbox].Path
	r.rawBases = []string{configured}
	home := codexHome(getenv, accountHome)
	if home != "" {
		if raw, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
			if doc, err := tomledit.Decode(string(raw)); err == nil {
				r.codexDoc = doc
			}
		}
	}

	// The protected homes: the passwd home, and $HOME when it differs and is not below the sandbox
	// base (a fake home a harness keeps under the sandbox base does not make that base a home ancestor).
	base, baseErr := filepath.EvalSymlinks(configured)
	var homes []string
	if accountHome != "" && filepath.IsAbs(accountHome) {
		homes = append(homes, best(accountHome))
	}
	if envHome := getenv("HOME"); envHome != "" && filepath.IsAbs(envHome) {
		real := best(envHome)
		if !containsString(homes, real) && !(baseErr == nil && within(real, base)) {
			homes = append(homes, real)
		}
	}
	r.homes = homes

	r.unusable = unusableBase(configured, base, baseErr, homes)
	if r.unusable == nil {
		r.sandboxBase = base
	}
	r.worktreeBase = []string{best(roots[crwconfig.RootWorktree].Path)}
	if home != "" {
		r.worktreeBase = append(r.worktreeBase, best(filepath.Join(home, "worktrees")))
	}
	r.projects = r.projectRoots(file, base, baseErr == nil && r.unusable == nil)
	return r
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// unusableBase judges the configured sandbox base by rules 1 to 4.
func unusableBase(configured, base string, baseErr error, homes []string) *Refusal {
	hint := "set paths.sandbox_root in the crw configuration (crw config show) to an existing dedicated directory outside the account home"
	refuse := func(why string) *Refusal {
		return &Refusal{Path: configured, Resolved: base, Rule: RuleSandboxRootUnusable, Hint: why + "; " + hint}
	}
	switch {
	case !filepath.IsAbs(configured):
		return refuse("sandbox_root is not an absolute path")
	case baseErr != nil:
		return refuse("sandbox_root does not exist")
	case base == "/":
		return refuse("sandbox_root is the filesystem root")
	}
	if rule := isHomeOrAbove(base, homes); rule != "" {
		return refuse("sandbox_root is the account home or a directory above it")
	}
	return nil
}

// projectRoots builds the known project roots from the explicit crw list and the Codex projects.
func (r *rules) projectRoots(file *crwconfig.File, base string, haveBase bool) []string {
	seen := map[string]bool{}
	admit := func(path string, explicit bool) {
		if !filepath.IsAbs(path) {
			return
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || real == "/" || isHomeOrAbove(real, r.homes) != "" {
			return
		}
		if !explicit && haveBase && within(real, base) && !hasGit(real) {
			return
		}
		seen[real] = true
	}
	if explicit, err := file.SandboxProjectRoots(); err == nil {
		for _, path := range explicit {
			admit(path, true)
		}
	}
	if table, ok := r.codexDoc["projects"].(map[string]any); ok {
		for path := range table {
			admit(path, false)
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (r *rules) allowed() []string {
	out := []string{}
	if r.sandboxBase != "" {
		out = append(out, r.sandboxBase)
	} else if len(r.rawBases) > 0 {
		out = append(out, r.rawBases...)
	}
	return append(out, r.worktreeBase...)
}

// enabled reports whether a Codex run with this sandbox has the sandbox on. An empty sandbox is
// whatever the Codex configuration defaults to; anything that cannot be decided is on.
func (r *rules) enabled(sandbox string) bool {
	switch sandbox {
	case DangerFullAccess, "dangerFullAccess", Bypass:
		return false
	case "":
		return r.defaultEnabled()
	}
	return true
}

func (r *rules) defaultEnabled() bool {
	pick := func(table map[string]any) (mode string, permissions bool, found bool) {
		if _, ok := table["default_permissions"].(string); ok {
			return "", true, true
		}
		if mode, ok := table["sandbox_mode"].(string); ok {
			return mode, false, true
		}
		return "", false, false
	}
	if profile, ok := r.codexDoc["profile"].(string); ok {
		if profiles, ok := r.codexDoc["profiles"].(map[string]any); ok {
			if table, ok := profiles[profile].(map[string]any); ok {
				if mode, permissions, found := pick(table); found {
					return permissions || (mode != DangerFullAccess)
				}
			}
		}
	}
	mode, permissions, found := pick(r.codexDoc)
	if !found {
		return true
	}
	return permissions || mode != DangerFullAccess
}

// Enabled reports whether a Codex run with this sandbox has the sandbox on, by the rules above.
func Enabled(sandbox string) bool { return load().enabled(sandbox) }

// refuseBasic judges rules 1 to 5 for a candidate and returns its real path.
func (r *rules) refuseBasic(candidate string) (*Refusal, string) {
	refusal := func(rule, resolved, project string) (*Refusal, string) {
		return &Refusal{Path: candidate, Resolved: resolved, Rule: rule, Project: project, Allowed: r.allowed()}, resolved
	}
	if !filepath.IsAbs(candidate) {
		return refusal(RuleRelative, "", "")
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return refusal(RuleMissing, "", "")
		}
		return refusal(RuleUnresolvable, "", "")
	}
	if real == "/" {
		return refusal(RuleFilesystemRoot, real, "")
	}
	if rule := isHomeOrAbove(real, r.homes); rule != "" {
		return refusal(rule, real, "")
	}
	for _, project := range r.projects {
		if below(project, real) {
			return refusal(RuleProjectAncestor, real, project)
		}
	}
	return nil, real
}

func (r *rules) unusableRefusal(candidate string) *Refusal {
	refusal := *r.unusable
	refusal.Path = candidate
	refusal.Allowed = r.allowed()
	return &refusal
}

// Check judges a working root. It returns nil, or a *Refusal. A sandbox that is off is outside the
// rule. Rule order (the first that applies is the answer): the sandbox base must be usable
// (sandbox_root_unusable); a relative path (relative); a path that does not exist (missing); the real
// path is judged from here on: the filesystem root; the account home and the directories above it;
// a directory above a known project root (project_ancestor); allowed are the sandbox base and
// everything below it, a project root and everything in it, and a linked worktree (a directory with
// a .git file) at or below a worktree base; anything else is outside_dedicated_root.
func Check(candidate, sandbox string) error {
	r := load()
	if !r.enabled(sandbox) {
		return nil
	}
	if r.unusable != nil {
		return r.unusableRefusal(candidate)
	}
	refusal, real := r.refuseBasic(candidate)
	if refusal != nil {
		return refusal
	}
	if within(real, r.sandboxBase) {
		return nil
	}
	for _, project := range r.projects {
		if within(real, project) {
			return nil
		}
	}
	if r.linkedWorktree(real) {
		return nil
	}
	return &Refusal{Path: candidate, Resolved: real, Rule: RuleOutsideDedicatedRoot, Allowed: r.allowed()}
}

// linkedWorktree reports whether real lies below a worktree base inside a directory that has a .git
// file (a linked worktree's marker).
func (r *rules) linkedWorktree(real string) bool {
	for _, base := range r.worktreeBase {
		if !below(real, base) {
			continue
		}
		for dir := real; below(dir, base); dir = filepath.Dir(dir) {
			if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && info.Mode().IsRegular() {
				return true
			}
		}
	}
	return false
}

// CheckWritable judges a writable root (the App Server runtimeWorkspaceRoots, a sandbox policy
// writable root, codex --add-dir) by rules 1 to 5 only: a writable root need not be in a dedicated
// root, but it cannot be the account home, a directory above it or above a project root.
func CheckWritable(root, sandbox string) error {
	r := load()
	if !r.enabled(sandbox) {
		return nil
	}
	if r.unusable != nil {
		return r.unusableRefusal(root)
	}
	if refusal, _ := r.refuseBasic(root); refusal != nil {
		return refusal
	}
	return nil
}

// CheckAll judges a working root and then each writable root. A caller that does not know the
// working root (a resume whose thread reports none and whose request names none) passes "" and has
// only its writable roots judged.
func CheckAll(cwd string, writable []string, sandbox string) error {
	if cwd != "" {
		if err := Check(cwd, sandbox); err != nil {
			return err
		}
	}
	for _, root := range writable {
		if err := CheckWritable(root, sandbox); err != nil {
			return err
		}
	}
	return nil
}

// CheckDestination judges a worktree that does not exist yet, before anything is created. The
// destination must be absolute; the real path of its nearest existing ancestor plus the rest of the
// path is what is judged. It is refused when that path is the filesystem root, the account home, a
// directory above it, or a worktree base itself, and when it is not strictly below a worktree base,
// the sandbox base or a known project root. The parent directory is not judged as a working root.
func CheckDestination(destination, sandbox string) error {
	r := load()
	if !r.enabled(sandbox) {
		return nil
	}
	if r.unusable != nil {
		return r.unusableRefusal(destination)
	}
	refusal := func(rule, resolved string) error {
		return &Refusal{Path: destination, Resolved: resolved, Rule: rule, Allowed: r.allowed()}
	}
	if !filepath.IsAbs(destination) {
		return refusal(RuleRelative, "")
	}
	resolved := nearestResolved(destination)
	if resolved == "/" {
		return refusal(RuleFilesystemRoot, resolved)
	}
	if rule := isHomeOrAbove(resolved, r.homes); rule != "" {
		return refusal(rule, resolved)
	}
	for _, base := range r.worktreeBase {
		if resolved == base {
			return refusal(RuleOutsideDedicatedRoot, resolved)
		}
	}
	bases := append([]string{r.sandboxBase}, r.worktreeBase...)
	bases = append(bases, r.projects...)
	for _, base := range bases {
		if below(resolved, base) {
			return nil
		}
	}
	return refusal(RuleOutsideDedicatedRoot, resolved)
}

// nearestResolved resolves the nearest existing ancestor of a path and appends the rest.
func nearestResolved(path string) string {
	rest := ""
	current := filepath.Clean(path)
	for {
		if real, err := filepath.EvalSymlinks(current); err == nil {
			if rest == "" {
				return real
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Join(current, rest)
		}
		if rest == "" {
			rest = filepath.Base(current)
		} else {
			rest = filepath.Join(filepath.Base(current), rest)
		}
		current = parent
	}
}

// SandboxOf reads the sandbox a host reported: a mode string ("workspace-write") or a policy object
// whose type is a camelCase mode ("workspaceWrite"). Anything else is Unknown.
func SandboxOf(reported any) string {
	switch value := reported.(type) {
	case string:
		if value != "" {
			return value
		}
	case map[string]any:
		if kind, ok := value["type"].(string); ok && kind != "" {
			return kind
		}
	}
	return Unknown
}

// RootsOf lists the writable roots a settings-shaped object (a resume's params, a recorded task
// settings row, a thread/read answer) carries: its runtimeWorkspaceRoots, the writableRoots of its
// sandbox policy (the object itself, or under "sandbox" or "sandboxPolicy") and the
// sandbox_workspace_write.writable_roots of its config. The object is a plain map; a nested object
// that is not one contributes nothing.
func RootsOf(object map[string]any) []string {
	var out []string
	add := func(list any) {
		switch roots := list.(type) {
		case []any:
			for _, root := range roots {
				if text, ok := root.(string); ok {
					out = append(out, text)
				}
			}
		case []string:
			out = append(out, roots...)
		}
	}
	add(object["runtimeWorkspaceRoots"])
	add(object["writableRoots"])
	for _, key := range []string{"sandbox", "sandboxPolicy"} {
		if policy, ok := object[key].(map[string]any); ok {
			add(policy["writableRoots"])
		}
	}
	if config, ok := object["config"].(map[string]any); ok {
		if section, ok := config["sandbox_workspace_write"].(map[string]any); ok {
			add(section["writable_roots"])
		}
	}
	return out
}
