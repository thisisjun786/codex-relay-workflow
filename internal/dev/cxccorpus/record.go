//go:build dev

package cxccorpus

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Epoch is the oracle's frozen clock: the fake-clock preload starts every Node process here and
// advances one millisecond per reading, and every given file's mtime is set to it.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// preload is the --import module every Node process of a recording runs first.
//   - Date is replaced so timestamps, millisecond-derived names and local dates are the same in
//     every recording; it advances one millisecond per reading so deadline loops still end.
//   - The network is closed: fetch, TCP/TLS connects and DNS lookups fail as an unreachable
//     host would, and each attempt is logged as a call ({"cmd": "fetch"|"connect"|"dns"}), so a
//     fixture shows what the oracle tried to reach. Unix sockets stay open.
const preload = `import { appendFileSync, readFileSync } from "node:fs";
import net from "node:net";
import dns from "node:dns";
const RealDate = Date;
let tick = RealDate.UTC(2026, 0, 1, 0, 0, 0, 0);
const now = () => tick++;
class FakeDate extends RealDate {
  constructor(...args) { if (args.length === 0) super(now()); else super(...args); }
  static now() { return now(); }
}
globalThis.Date = FakeDate;
const log = (cmd, argv) => {
  try { appendFileSync(process.env.CXC_REC_LOG, JSON.stringify({ cmd, argv, cwd: process.cwd() }) + "\n"); } catch {}
};
let scripted = {};
try { scripted = JSON.parse(readFileSync(process.env.CXC_REC_DIR + "/fetch.json", "utf8")); } catch {}
globalThis.fetch = async (input, init) => {
  const url = typeof input === "string" ? input : (input && input.url) || String(input);
  log("fetch", [(init && init.method) || "GET", url]);
  const reply = scripted[url];
  if (!reply) throw new TypeError("fetch failed");
  return new Response(reply.body, { status: reply.status || 200 });
};
const connect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function (...args) {
  const first = args[0];
  const options = Array.isArray(first) ? first[0] : first;
  if (options && typeof options === "object" && typeof options.path === "string") return connect.apply(this, args);
  if (typeof options === "string" && isNaN(Number(options))) return connect.apply(this, args);
  log("connect", [String((options && options.host) || args[1] || "localhost"), String((options && options.port) || options)]);
  process.nextTick(() => this.destroy(Object.assign(new Error("connect ENETUNREACH (closed by the recorder)"), { code: "ENETUNREACH" })));
  return this;
};
const refuse = (name) => (host, ...rest) => {
  log("dns", [String(host)]);
  const cb = rest.find((x) => typeof x === "function");
  const err = Object.assign(new Error("getaddrinfo ENOTFOUND " + host), { code: "ENOTFOUND", hostname: host });
  if (cb) { process.nextTick(() => cb(err)); return; }
  return Promise.reject(err);
};
dns.lookup = refuse("lookup");
dns.promises.lookup = refuse("lookup");
`

// stubProgram is every fake external program: it appends {cmd, argv, cwd} to the call log and
// answers what the scenario scripted (in <REC>/stubs/<name>.json), else exits 127.
const stubProgram = `#!%s
const fs = require("node:fs");
const path = require("node:path");
const name = path.basename(process.argv[1]);
fs.appendFileSync(process.env.CXC_REC_LOG, JSON.stringify({ cmd: name, argv: process.argv.slice(2), cwd: process.cwd() }) + "\n");
let script = null;
try { script = JSON.parse(fs.readFileSync(path.join(process.env.CXC_REC_DIR, "stubs", name + ".json"), "utf8")); } catch {}
if (!script) { process.stderr.write("stub " + name + ": not scripted\n"); process.exit(127); }
const argv = process.argv.slice(2);
for (const c of script.cases || []) {
  if (c.argv.every((a, i) => argv[i] === a)) { script = c; break; }
}
if (script.stdout) process.stdout.write(script.stdout);
if (script.stderr) process.stderr.write(script.stderr);
process.exitCode = script.exit;
`

// gitWrapper logs {cmd: "git", argv, cwd} and runs the real git with the same stdio.
const gitWrapper = `#!%s
const fs = require("node:fs");
const { spawnSync } = require("node:child_process");
fs.appendFileSync(process.env.CXC_REC_LOG, JSON.stringify({ cmd: "git", argv: process.argv.slice(2), cwd: process.cwd() }) + "\n");
const r = spawnSync(%q, process.argv.slice(2), { stdio: "inherit" });
process.exit(r.status === null ? 1 : r.status);
`

// sqliteSeeder creates the given databases with node:sqlite (Node is the oracle's runtime; the
// recorder already needs it).
const sqliteSeeder = `const { DatabaseSync } = require("node:sqlite");
const seeds = JSON.parse(require("node:fs").readFileSync(0, "utf8"));
for (const [file, statements] of Object.entries(seeds)) {
  const db = new DatabaseSync(file);
  for (const sql of statements) db.exec(sql);
  db.close();
}
`

// sqliteDumper prints a database as {"tables": [{"name", "sql", "rows"}]}: the schema text and
// every row in rowid order, so a fixture compares contents rather than page bytes.
const sqliteDumper = `const { DatabaseSync } = require("node:sqlite");
const db = new DatabaseSync(process.argv[1], { readOnly: true });
const tables = db.prepare("SELECT name, sql FROM sqlite_master WHERE type IN ('table','index','view','trigger') AND sql IS NOT NULL ORDER BY type, name").all();
const out = [];
for (const t of tables) {
  let rows = null;
  if (/^CREATE TABLE/i.test(t.sql) && !/VIRTUAL TABLE/i.test(t.sql)) {
    try { rows = db.prepare("SELECT * FROM \"" + t.name.replace(/"/g, '""') + "\"").all(); } catch { rows = null; }
  }
  out.push({ name: t.name, sql: t.sql, rows });
}
db.close();
process.stdout.write(JSON.stringify({ tables: out }));
`

// DefaultStubs are on PATH in every case unless a scenario removes one with a null stub.
var DefaultStubs = []string{"codex", "ocx", "gh", "uv", "sg"}

// Recorder runs scenarios against one oracle checkout.
type Recorder struct {
	Oracle  string // the extracted v0.2.40 tree
	Node    string // absolute node executable
	Git     string // absolute git executable
	Scratch string // parent of every case root
	Decls   map[string]Declaration
	Rules   *Normaliser
	Timeout time.Duration
	Keep    bool // keep case roots for inspection
}

// Declaration is one registered hook leg (hook-declarations.json).
type Declaration struct {
	Leg     string   `json:"leg"`
	File    string   `json:"file"`
	Event   string   `json:"event"`
	Matcher string   `json:"matcher,omitempty"`
	Command string   `json:"command"`
	Entry   string   `json:"entry"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
	Status  string   `json:"statusMessage"`
}

// caseRoot is one scenario's isolated tree.
type caseRoot struct {
	root string
	env  []string
}

// gitEnv fixes git's identity, dates and configuration.
var gitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
	"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
}

// Check refuses a recorder whose scratch root could reach real state: it must be absolute,
// outside the operator's home state directories, outside any git checkout, and the oracle must
// carry the tagged manifest.
func (r *Recorder) Check() error {
	if !filepath.IsAbs(r.Scratch) || !filepath.IsAbs(r.Oracle) || !filepath.IsAbs(r.Node) || !filepath.IsAbs(r.Git) {
		return errors.New("scratch, oracle, node and git must be absolute paths")
	}
	scratch, err := filepath.EvalSymlinks(r.Scratch)
	if err != nil {
		return fmt.Errorf("scratch root: %w", err)
	}
	if home := osGetenv("HOME"); home != "" {
		for _, live := range []string{".codex", ".codexclaw", ".local/share/crw-runtime", ".local/state"} {
			if isWithin(scratch, filepath.Join(home, live)) {
				return fmt.Errorf("scratch root %s is inside live state %s", scratch, filepath.Join(home, live))
			}
		}
		if scratch == home {
			return errors.New("scratch root is the operator's home")
		}
	}
	for dir := scratch; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("scratch root %s is inside the git checkout %s", scratch, dir)
		}
		if dir == "/" {
			break
		}
	}
	manifest, err := os.ReadFile(filepath.Join(r.Oracle, "plugins/codexclaw/.codex-plugin/plugin.json"))
	if err != nil {
		return fmt.Errorf("oracle: %w", err)
	}
	if !bytes.Contains(manifest, []byte(`"version": "0.2.40+codex.20260929183231"`)) {
		return errors.New("oracle: plugins/codexclaw/.codex-plugin/plugin.json is not the v0.2.40 manifest")
	}
	return nil
}

func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// newCase builds the isolated root: the five roots, the stub and tool directories, the git
// configuration, and the environment every process of the case gets (nothing inherited).
func (r *Recorder) newCase(s Scenario) (*caseRoot, error) {
	// A fixed-width name: a byte count or a cut that depends on an expanded path (a file
	// holding ${WS}, a line truncated after the plugin root) is then the same in every case.
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	root := filepath.Join(r.Scratch, "cxc-rec-"+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, err
	}
	c := &caseRoot{root: root}
	for _, dir := range append(append([]string{}, Roots...), "stubs", "bin", ".rec", ".rec/stubs") {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return c, err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "gitconfig"), []byte("[user]\n\tname = fixture\n\temail = fixture@example.invalid\n[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		return c, err
	}
	if err := os.Symlink(r.Node, filepath.Join(root, "bin", "node")); err != nil {
		return c, err
	}
	// git on PATH is the real git behind a logging wrapper, so the oracle's git argv is a call
	// like any stub's while git itself does the work.
	if err := os.WriteFile(filepath.Join(root, "bin", "git"), []byte(fmt.Sprintf(gitWrapper, r.Node, r.Git)), 0o755); err != nil {
		return c, err
	}
	if len(s.Given.Fetch) > 0 {
		raw, _ := json.Marshal(s.Given.Fetch)
		if err := os.WriteFile(filepath.Join(root, ".rec", "fetch.json"), raw, 0o644); err != nil {
			return c, err
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".rec", "preload.mjs"), []byte(preload), 0o644); err != nil {
		return c, err
	}
	if err := os.WriteFile(filepath.Join(root, ".rec", "calls.jsonl"), nil, 0o644); err != nil {
		return c, err
	}
	stubs := map[string]*Stub{}
	for _, name := range DefaultStubs {
		stubs[name] = &Stub{Exit: 127}
	}
	for name, stub := range s.Given.Stubs {
		if stub == nil {
			delete(stubs, name)
			continue
		}
		stubs[name] = stub
	}
	for name, stub := range stubs {
		if err := os.WriteFile(filepath.Join(root, "stubs", name), []byte(fmt.Sprintf(stubProgram, r.Node)), 0o755); err != nil {
			return c, err
		}
		if _, scripted := s.Given.Stubs[name]; !scripted {
			continue // unscripted default: exits 127 after logging
		}
		raw, _ := json.Marshal(stub)
		if err := os.WriteFile(filepath.Join(root, ".rec", "stubs", name+".json"), raw, 0o644); err != nil {
			return c, err
		}
	}
	c.env = []string{
		"PATH=" + filepath.Join(root, "stubs") + ":" + filepath.Join(root, "bin"),
		"HOME=" + filepath.Join(root, "home"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"TZ=UTC", "LANG=C.UTF-8",
		"CODEX_HOME=" + filepath.Join(root, "codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(root, "codex"),
		"CODEXCLAW_HOME=" + filepath.Join(root, "cxc"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "home", ".config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "home", ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(root, "home", ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "home", ".cache"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "tmp", "run"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"),
		"NODE_OPTIONS=--import=file://" + filepath.Join(root, ".rec", "preload.mjs"),
		"CXC_REC_LOG=" + filepath.Join(root, ".rec", "calls.jsonl"),
		"CXC_REC_DIR=" + filepath.Join(root, ".rec"),
	}
	c.env = append(c.env, gitEnv...)
	return c, nil
}

// expand substitutes the case's placeholders into given text, argv and stdin.
func (r *Recorder) expand(c *caseRoot, text string) string {
	return strings.NewReplacer(
		"${PLUGIN_ROOT}", filepath.Join(r.Oracle, "plugins", "codexclaw"),
		"${CXC_ROOT}", r.Oracle,
		"${WS}", filepath.Join(c.root, "ws"),
		"${CODEX_HOME}", filepath.Join(c.root, "codex"),
		"${CXC_HOME}", filepath.Join(c.root, "cxc"),
		"${HOME}", filepath.Join(c.root, "home"),
		"${TMP}", filepath.Join(c.root, "tmp"),
		"${STUBS}", filepath.Join(c.root, "stubs"),
		"${BIN}", filepath.Join(c.root, "bin"),
		"${REC}", filepath.Join(c.root, ".rec"),
		"${ROOT}", c.root,
		"${NODE}", r.Node,
	).Replace(text)
}

func (r *Recorder) bindings(c *caseRoot) []Binding {
	return []Binding{
		{"${PLUGIN_ROOT}", filepath.Join(r.Oracle, "plugins", "codexclaw")},
		{"${CXC_ROOT}", r.Oracle},
		{"${WS}", filepath.Join(c.root, "ws")},
		{"${CODEX_HOME}", filepath.Join(c.root, "codex")},
		{"${CXC_HOME}", filepath.Join(c.root, "cxc")},
		{"${HOME}", filepath.Join(c.root, "home")},
		{"${TMP}", filepath.Join(c.root, "tmp")},
		{"${STUBS}", filepath.Join(c.root, "stubs")},
		{"${BIN}", filepath.Join(c.root, "bin")},
		{"${REC}", filepath.Join(c.root, ".rec")},
		{"${ROOT}", c.root},
		{"${NODE}", r.Node},
	}
}

// casePath resolves a given path, refusing one outside the scenario roots.
func casePath(c *caseRoot, rel string) (string, error) {
	clean := filepath.Clean(rel)
	first, _, _ := strings.Cut(clean, "/")
	known := false
	for _, root := range Roots {
		known = known || first == root
	}
	if !known || strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("given path %q is not under one of %v", rel, Roots)
	}
	return filepath.Join(c.root, clean), nil
}

// setUp writes the given state.
func (r *Recorder) setUp(c *caseRoot, g Given) error {
	for _, dir := range g.Dirs {
		path, err := casePath(c, dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	write := func(rel string, data []byte) error {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o644)
	}
	for _, rel := range sortedKeys(g.Files) {
		if err := write(rel, []byte(r.expand(c, g.Files[rel]))); err != nil {
			return err
		}
	}
	for _, rel := range sortedKeys(g.JSON) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, g.JSON[rel]); err != nil {
			return fmt.Errorf("given json %s: %w", rel, err)
		}
		if err := write(rel, []byte(r.expand(c, compact.String())+"\n")); err != nil {
			return err
		}
	}
	if len(g.SQLite) > 0 {
		seeds := map[string][]string{}
		for rel, statements := range g.SQLite {
			path, err := casePath(c, rel)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			for _, statement := range statements {
				seeds[path] = append(seeds[path], r.expand(c, statement))
			}
		}
		raw, _ := json.Marshal(seeds)
		cmd := exec.Command(r.Node, "-e", sqliteSeeder)
		cmd.Env = withoutClock(c.env)
		cmd.Dir = c.root
		cmd.Stdin = bytes.NewReader(raw)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("seed sqlite: %w: %s", err, out)
		}
	}
	for _, rel := range sortedKeys(g.Symlinks) {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(r.expand(c, g.Symlinks[rel]), path); err != nil {
			return err
		}
	}
	if g.Git != nil {
		dir := g.Git.Dir
		if dir == "" {
			dir = "ws"
		}
		ws, err := casePath(c, dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(ws, 0o755); err != nil {
			return err
		}
		steps := [][]string{{"init", "-q"}}
		if g.Git.Origin != "" {
			steps = append(steps, []string{"remote", "add", "origin", g.Git.Origin})
		}
		if g.Git.Commit != "" {
			steps = append(steps, []string{"add", "-A"}, []string{"commit", "-q", "--allow-empty", "-m", g.Git.Commit})
		}
		for _, wt := range g.Git.Worktrees {
			path, err := casePath(c, wt.Path)
			if err != nil {
				return err
			}
			steps = append(steps, []string{"worktree", "add", "-q", "-b", wt.Branch, path})
		}
		for _, args := range steps {
			cmd := exec.Command(r.Git, args...)
			cmd.Dir = ws
			cmd.Env = c.env
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
	}
	for _, rel := range sortedKeys(g.Modes) {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		if err := os.Chmod(path, fs.FileMode(g.Modes[rel])); err != nil {
			return err
		}
	}
	// Every given entry carries the frozen clock's time, so age and staleness readings agree
	// with the oracle's Date.
	err := filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		return os.Chtimes(path, Epoch, Epoch)
	})
	if err != nil {
		return err
	}
	for _, rel := range sortedKeys(g.Mtimes) {
		path, err := casePath(c, rel)
		if err != nil {
			return err
		}
		when, err := time.Parse(time.RFC3339, g.Mtimes[rel])
		if err != nil {
			return fmt.Errorf("mtime %s: %w", rel, err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			return err
		}
	}
	return nil
}

func withoutClock(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "NODE_OPTIONS=") {
			out = append(out, kv)
		}
	}
	return out
}

// rawResult is one step before normalisation.
type rawResult struct {
	exit           int
	signal         string
	timeout        bool
	stdout, stderr string
}

// command builds one step's process.
func (r *Recorder) command(ctx context.Context, c *caseRoot, s Scenario, step Step) (*exec.Cmd, []byte, error) {
	plugin := filepath.Join(r.Oracle, "plugins", "codexclaw")
	var argv []string
	dir := filepath.Join(c.root, "ws")
	extra := []string{}
	set := 0
	switch {
	case step.Hook != "":
		set++
		decl, ok := r.Decls[step.Hook]
		if !ok {
			return nil, nil, fmt.Errorf("hook leg %q is not a registered declaration", step.Hook)
		}
		argv = append([]string{r.Node, filepath.Join(plugin, decl.Entry)}, decl.Args...)
		extra = append(extra, "PLUGIN_ROOT="+plugin)
	}
	if step.CLI != nil {
		set++
		bin := filepath.Join(r.Oracle, "bin", "codexclaw.mjs")
		if step.Payload {
			bin = filepath.Join(plugin, "bin", "cxc.mjs")
		}
		argv = append([]string{r.Node, bin}, step.CLI...)
	}
	if step.Node != nil {
		set++
		argv = append([]string{r.Node, filepath.Join(r.Oracle, step.Node[0])}, step.Node[1:]...)
	}
	var stdin []byte
	if step.MCP != nil {
		set++
		argv = []string{r.Node, filepath.Join(plugin, "components", "subagent-config", "dist", "mcp.js")}
		dir = plugin
		for _, msg := range step.MCP {
			var compact bytes.Buffer
			if err := json.Compact(&compact, msg); err != nil {
				return nil, nil, err
			}
			stdin = append(stdin, compact.Bytes()...)
			stdin = append(stdin, '\n')
		}
	}
	if set != 1 {
		return nil, nil, errors.New("a step sets exactly one of hook, cli, node, mcp")
	}
	for i := range argv {
		argv[i] = r.expand(c, argv[i])
	}
	if step.Stdin != nil {
		var text string
		if json.Unmarshal(step.Stdin, &text) != nil {
			var compact bytes.Buffer
			if err := json.Compact(&compact, step.Stdin); err != nil {
				return nil, nil, err
			}
			text = compact.String()
		}
		stdin = []byte(r.expand(c, text))
	}
	if step.StdinPad > 0 {
		stdin = append(stdin, bytes.Repeat([]byte{' '}, step.StdinPad)...)
	}
	if step.Cwd != "" {
		path, err := casePath(c, step.Cwd)
		if err != nil {
			return nil, nil, err
		}
		dir = path
	}
	env := append(append([]string{}, c.env...), extra...)
	for _, key := range sortedKeys(s.Given.Env) {
		env = setEnv(env, key, r.expand(c, s.Given.Env[key]))
	}
	for _, key := range sortedKeys(step.Env) {
		env = setEnv(env, key, r.expand(c, step.Env[key]))
	}
	for _, key := range step.Unset {
		env = setEnv(env, key, "\x00unset")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, stdin, nil
}

func setEnv(env []string, key, value string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	if value == "\x00unset" {
		return out
	}
	return append(out, key+"="+value)
}

func (r *Recorder) runStep(c *caseRoot, s Scenario, step Step) (rawResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.Timeout)
	defer cancel()
	cmd, stdin, err := r.command(ctx, c, s, step)
	if err != nil {
		return rawResult{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	res := rawResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() != nil {
		res.timeout = true
	}
	// Whatever the step left in its process group (a detached child, a stray stub) dies with it.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		status := exitErr.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			res.signal = status.Signal().String()
			res.exit = -1
		} else {
			res.exit = status.ExitStatus()
		}
	case res.timeout:
		res.exit = -1
	default:
		return res, err
	}
	return res, nil
}

// Record runs a scenario once in a fresh case root and returns its normalised fixture. A case
// root that cannot be removed afterwards fails the recording, so none is left behind unnoticed.
func (r *Recorder) Record(s Scenario) (fixture Fixture, err error) {
	c, err := r.newCase(s)
	if c != nil && !r.Keep {
		defer func() {
			if rmErr := removeTree(c.root); rmErr != nil && err == nil {
				err = fmt.Errorf("%s: remove case root %s: %w", s.ID, c.root, rmErr)
			}
		}()
	}
	if err != nil {
		return Fixture{}, err
	}
	if err := r.setUp(c, s.Given); err != nil {
		return Fixture{}, fmt.Errorf("%s: given: %w", s.ID, err)
	}
	session := r.Rules.NewSession(r.bindings(c))
	var results []StepResult
	for i, step := range s.Steps {
		if step.Write != nil {
			if step.Hook != "" || step.CLI != nil || step.Node != nil || step.MCP != nil {
				return Fixture{}, fmt.Errorf("%s: step %d: a write step runs nothing else", s.ID, i)
			}
			for _, rel := range sortedKeys(step.Write) {
				path, err := casePath(c, rel)
				if err == nil {
					err = os.MkdirAll(filepath.Dir(path), 0o755)
				}
				if err == nil {
					err = os.WriteFile(path, []byte(r.expand(c, step.Write[rel])), 0o644)
				}
				if err != nil {
					return Fixture{}, fmt.Errorf("%s: step %d: %w", s.ID, i, err)
				}
			}
			results = append(results, StepResult{Action: "write", StdoutForm: "empty"})
			continue
		}
		raw, err := r.runStep(c, s, step)
		if err != nil {
			return Fixture{}, fmt.Errorf("%s: step %d: %w", s.ID, i, err)
		}
		results = append(results, shapeStep(session, raw))
	}
	observe := s.Observe
	if len(observe) == 0 {
		observe = DefaultObserve
	}
	tree, err := r.observeTree(c, session, observe)
	if err != nil {
		return Fixture{}, fmt.Errorf("%s: observe: %w", s.ID, err)
	}
	calls, err := readCalls(c, session)
	if err != nil {
		return Fixture{}, fmt.Errorf("%s: calls: %w", s.ID, err)
	}
	exit := 0
	if len(results) > 0 {
		exit = results[len(results)-1].Exit
	}
	return Fixture{
		Oracle: OracleTag + " " + OracleCommit,
		Covers: s.Covers,
		Note:   s.Note,
		Given:  s.Given,
		Run:    RunBlock{Kind: RunKind, Steps: s.Steps, Observe: s.Observe},
		Expect: Expect{Exit: exit, Steps: results, Tree: tree, Calls: calls},
	}, nil
}

// removeTree deletes a case root, first making read-only directories writable again.
func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// shapeStep normalises a step and classifies its stdout (classify).
func shapeStep(s *Session, raw rawResult) StepResult {
	res := StepResult{Exit: raw.exit, Signal: raw.signal, Timeout: raw.timeout, Stderr: s.Stderr(raw.stderr)}
	out := s.Text(raw.stdout)
	form, doc, lines := classify(out)
	res.StdoutForm = form
	switch form {
	case "empty":
	case "text":
		res.Stdout = &out
	case "jsonl":
		res.StdoutJSONL = lines
	default:
		res.StdoutJSON = doc
	}
	return res
}

// classify names how a text is framed, so a fixture can hold the JSON readable and a replay can
// still rebuild the exact bytes: empty; json (one compact document, as JSON.stringify prints it)
// and json-line (the same plus a newline); json-pretty-nonl and json-pretty (two-space indented,
// as JSON.stringify(v, null, 2) prints it, without and with a newline); jsonl (two or more compact
// documents, one per line, newline-terminated); text (anything else, kept verbatim).
func classify(out string) (string, json.RawMessage, []json.RawMessage) {
	if out == "" {
		return "empty", nil, nil
	}
	body, newline := strings.CutSuffix(out, "\n")
	if json.Valid([]byte(body)) {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(body)) == nil && compact.String() == body {
			if newline {
				return "json-line", indent(body), nil
			}
			return "json", indent(body), nil
		}
		if pretty := indent(body); string(pretty) == body {
			if newline {
				return "json-pretty", pretty, nil
			}
			return "json-pretty-nonl", pretty, nil
		}
	}
	if newline && strings.Contains(body, "\n") {
		var docs []json.RawMessage
		for _, line := range strings.Split(body, "\n") {
			var compact bytes.Buffer
			if !json.Valid([]byte(line)) || json.Compact(&compact, []byte(line)) != nil || compact.String() != line {
				return "text", nil, nil
			}
			docs = append(docs, indent(line))
		}
		return "jsonl", nil, docs
	}
	return "text", nil, nil
}

// indent re-indents a JSON document keeping its key order and escapes.
func indent(doc string) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(strings.TrimSpace(doc)), "", "  "); err != nil {
		return json.RawMessage(doc)
	}
	return json.RawMessage(buf.Bytes())
}

// observeTree records every entry under the observed roots, keyed by its normalised path.
func (r *Recorder) observeTree(c *caseRoot, s *Session, roots []string) (map[string]Entry, error) {
	includeObservations := false
	type found struct {
		rel, key string
		path     string
		d        fs.DirEntry
	}
	var all []found
	for _, root := range roots {
		if root == HookObservations {
			includeObservations = true
		}
	}
	for _, root := range roots {
		// A root inside another listed root is walked by that one.
		nested := false
		for _, other := range roots {
			nested = nested || (other != root && strings.HasPrefix(root, other+"/"))
		}
		if nested {
			continue
		}
		if _, err := casePath(c, root); err != nil {
			return nil, fmt.Errorf("observe: %w", err)
		}
		base := filepath.Join(c.root, root)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if path == base {
				return nil
			}
			rel, _ := filepath.Rel(c.root, path)
			if d.IsDir() && d.Name() == ".git" {
				// Git's own files (index timestamps, object packing) are not CXC's output.
				all = append(all, found{rel: rel, path: path, d: d})
				return fs.SkipDir
			}
			if !includeObservations && (rel == HookObservations || strings.HasPrefix(rel, HookObservations+"/")) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			all = append(all, found{rel: rel, path: path, d: d})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Keys are normalised paths, and aliases are numbered in key order: sort by the path with
	// the placeholders and text rules applied (a throwaway session), so a random component does
	// not decide the numbering.
	probe := s.n.NewSession(s.bindings)
	for i := range all {
		all[i].key = probe.Text(all[i].rel)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return all[i].rel < all[j].rel
	})
	tree := map[string]Entry{}
	for _, f := range all {
		info, err := os.Lstat(f.path)
		if err != nil {
			return nil, err
		}
		key := s.Text(f.rel)
		entry := Entry{Mode: fmt.Sprintf("%04o", info.Mode().Perm())}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(f.path)
			if err != nil {
				return nil, err
			}
			entry.Type, entry.Mode, entry.Target = "symlink", "", s.Text(target)
		case info.IsDir():
			entry.Type = "dir"
		case info.Mode().IsRegular():
			entry.Type = "file"
			raw, err := os.ReadFile(f.path)
			if err != nil {
				return nil, err
			}
			if base := f.path; strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") || strings.HasSuffix(base, "-journal") {
				if _, err := os.Stat(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, "-wal"), "-shm"), "-journal")); err == nil {
					// SQLite's own sidecar files carry salts and frame counters; their
					// presence is the observation.
					entry.Form = "sqlite-sidecar"
					break
				}
			}
			if bytes.HasPrefix(raw, []byte("SQLite format 3\x00")) {
				if dump, err := r.dumpSQLite(f.path); err == nil {
					if text := s.Text(dump); json.Valid([]byte(text)) {
						entry.Form, entry.JSON = "sqlite", indent(text)
					} else {
						entry.Form, entry.Text = "sqlite-text", &text
					}
					break
				}
			}
			fileContent(&entry, s, raw)
		default:
			entry.Type = "other"
		}
		tree[key] = entry
	}
	return tree, nil
}

func fileContent(entry *Entry, s *Session, raw []byte) {
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		sum := sha256.Sum256(raw)
		size := int64(len(raw))
		entry.Form, entry.SHA256, entry.Size = "binary", hex.EncodeToString(sum[:]), &size
		return
	}
	text := s.Text(string(raw))
	form, doc, lines := classify(text)
	entry.Form = form
	switch form {
	case "empty":
	case "text":
		entry.Text = &text
	case "jsonl":
		entry.JSONL = lines
	default:
		entry.JSON = doc
	}
}

// dumpSQLite reads a database the oracle wrote, without the frozen clock.
func (r *Recorder) dumpSQLite(path string) (string, error) {
	cmd := exec.Command(r.Node, "-e", sqliteDumper, path)
	cmd.Env = []string{"PATH=/nonexistent", "HOME=/nonexistent"}
	out, err := cmd.Output()
	return string(out), err
}

// readCalls is the stub call log, normalised.
func readCalls(c *caseRoot, s *Session) ([]Call, error) {
	raw, err := os.ReadFile(filepath.Join(c.root, ".rec", "calls.jsonl"))
	if err != nil {
		return nil, err
	}
	calls := []Call{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var call Call
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			return nil, err
		}
		for i := range call.Argv {
			call.Argv[i] = s.Text(call.Argv[i])
		}
		call.Cwd = s.Text(call.Cwd)
		calls = append(calls, call)
	}
	return calls, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
