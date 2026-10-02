//go:build dev

package cxccorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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

func withoutClock(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "NODE_OPTIONS=") {
			out = append(out, kv)
		}
	}
	return out
}

// homeVar is the variable that names the CXC home root in an oracle process.
const homeVar = "CODEXCLAW_HOME"

// osGetenv is os.Getenv; a seam for the safety checks' tests.
var osGetenv = os.Getenv

// A Recorder is the Runtime of the Node oracle.
var _ Runtime = (*Recorder)(nil)

// newCase makes a case root and readies it for the oracle (constants.go runs outside a scenario).
func (r *Recorder) newCase(s Scenario) (*Case, error) {
	c, err := NewCase(r.Scratch, homeVar, s.Given)
	if err != nil {
		return c, err
	}
	return c, c.Prepare(r, s)
}

// Setup puts the oracle's programs in the case: node, git behind a logging wrapper (so the
// oracle's git argv is a call like any stub's while git itself does the work), the stubs, the
// scripted network replies and the fake-clock preload, with the variables that point at them.
func (r *Recorder) Setup(c *Case, s Scenario) error {
	if err := os.Symlink(r.Node, filepath.Join(c.Root, "bin", "node")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.Root, "bin", "git"), []byte(fmt.Sprintf(gitWrapper, r.Node, r.Git)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.Root, ".rec", "preload.mjs"), []byte(preload), 0o644); err != nil {
		return err
	}
	err := InstallStubs(c, s.Given, func(name string) error {
		return os.WriteFile(filepath.Join(c.Root, "stubs", name), []byte(fmt.Sprintf(stubProgram, r.Node)), 0o755)
	})
	c.Env = append(c.Env,
		"NODE_OPTIONS=--import=file://"+filepath.Join(c.Root, ".rec", "preload.mjs"),
		"CXC_REC_LOG="+filepath.Join(c.Root, ".rec", "calls.jsonl"),
		"CXC_REC_DIR="+filepath.Join(c.Root, ".rec"),
	)
	return err
}

// Bindings are the case's placeholder paths and the oracle's own.
func (r *Recorder) Bindings(c *Case) []Binding {
	plugin := filepath.Join(r.Oracle, "plugins", "codexclaw")
	all := []Binding{{"${PLUGIN_ROOT}", plugin}, {"${CXC_ROOT}", r.Oracle}}
	all = append(all, c.Bindings()...)
	return append(all, Binding{"${NODE}", r.Node})
}

// Command is the oracle's invocation of one step.
func (r *Recorder) Command(c *Case, s Scenario, step Step) (Invocation, error) {
	plugin := filepath.Join(r.Oracle, "plugins", "codexclaw")
	var inv Invocation
	set := 0
	if step.Hook != "" {
		set++
		decl, ok := r.Decls[step.Hook]
		if !ok {
			return inv, fmt.Errorf("hook leg %q is not a registered declaration", step.Hook)
		}
		inv.Argv = append([]string{r.Node, filepath.Join(plugin, decl.Entry)}, decl.Args...)
		inv.Env = append(inv.Env, "PLUGIN_ROOT="+plugin)
	}
	if step.CLI != nil {
		set++
		bin := filepath.Join(r.Oracle, "bin", "codexclaw.mjs")
		if step.Payload {
			bin = filepath.Join(plugin, "bin", "cxc.mjs")
		}
		inv.Argv = append([]string{r.Node, bin}, step.CLI...)
	}
	if step.Node != nil {
		set++
		inv.Argv = append([]string{r.Node, filepath.Join(r.Oracle, step.Node[0])}, step.Node[1:]...)
	}
	if step.MCP != nil {
		set++
		inv.Argv = []string{r.Node, filepath.Join(plugin, "components", "subagent-config", "dist", "mcp.js")}
		inv.Dir = plugin
		for _, msg := range step.MCP {
			var compact bytes.Buffer
			if err := json.Compact(&compact, msg); err != nil {
				return inv, err
			}
			inv.Stdin = append(inv.Stdin, compact.Bytes()...)
			inv.Stdin = append(inv.Stdin, '\n')
		}
	}
	if set != 1 {
		return inv, errors.New("a step sets exactly one of hook, cli, node, mcp")
	}
	return inv, nil
}

// SeedSQLite creates the given databases with node:sqlite (Node is the oracle's runtime; the
// recorder already needs it), without the frozen clock.
func (r *Recorder) SeedSQLite(c *Case, seeds map[string][]string) error {
	raw, _ := json.Marshal(seeds)
	cmd := exec.Command(r.Node, "-e", sqliteSeeder)
	cmd.Env = withoutClock(c.Env)
	cmd.Dir = c.Root
	cmd.Stdin = bytes.NewReader(raw)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("seed sqlite: %w: %s", err, out)
	}
	return nil
}

// GitPath is the real git behind the logging wrapper.
func (r *Recorder) GitPath() string { return r.Git }

// HookObservations is the diagnostic store every hook invocation writes.
func (r *Recorder) HookObservations() string { return HookObservations }

// Record runs a scenario once in a fresh case root and returns its normalised fixture.
func (r *Recorder) Record(s Scenario) (Fixture, error) {
	expect, err := RunScenario(r, RunOptions{Scratch: r.Scratch, HomeVar: homeVar, Rules: r.Rules, Timeout: r.Timeout, Keep: r.Keep}, s)
	if err != nil {
		return Fixture{}, err
	}
	return Fixture{
		Oracle: OracleTag + " " + OracleCommit,
		Covers: s.Covers,
		Note:   s.Note,
		Given:  s.Given,
		Run:    RunBlock{Kind: RunKind, Steps: s.Steps, Observe: s.Observe},
		Expect: expect,
	}, nil
}

// DumpSQLite reads a database the oracle wrote, without the frozen clock.
func (r *Recorder) DumpSQLite(path string) (string, error) {
	cmd := exec.Command(r.Node, "-e", sqliteDumper, path)
	cmd.Env = []string{"PATH=/nonexistent", "HOME=/nonexistent"}
	out, err := cmd.Output()
	return string(out), err
}
