package doctor_test

// This file replays testdata/harness/hooks/oracle.json, recorded by record-hooks.mjs from CXC v0.2.40
// (3c1459ac), over harness.ReadHookObservations and the two hook checks, and adds the cases the oracle
// cannot give: records the Go writer (harness.RecordInvocation) leaves, and a manifest that links out of the
// plugin. It ports the doctor parts of hook-observation.test.ts (:91-97, :124-135) and hook-trust.test.ts
// (:267-290, :564-615); cxc-ops.test.ts reaches the checks only through runDoctor, which belongs to the
// command that assembles the report.
//
// The oracle writes <codexHome>/codexclaw/... and names a component script as the entrypoint; the Go writer
// writes <codexHome>/crw/... and names the manifest, so a recorded record is written into the Go layout
// (the replay maps the two names and recomputes the digests) and the evidence's entrypoint is mapped back.
// An expectation of a check goes through the names decision too: `cxc hooks retrust` is
// `crw doctor retrust` (the cli table).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

type hooksOracle struct {
	T0       int64             `json:"t0"`
	Manifest string            `json:"manifest"`
	Reader   []hooksReaderCase `json:"reader"`
	Trust    []hooksTrustCase  `json:"trust"`
}

type hooksReaderCase struct {
	Name     string       `json:"name"`
	Mutation string       `json:"mutation"`
	Files    []hooksFile  `json:"files"`
	Queries  []hooksQuery `json:"queries"`
}

type hooksFile struct {
	Actor struct {
		Session string
		Agent   *string
	} `json:"actor"`
	Slot      []string `json:"slot"`
	Name      string   `json:"name"`
	Text      string   `json:"text"`
	SymlinkTo []string `json:"symlinkTo"`
	PadTo     int      `json:"padTo"`
	Dir, Fifo bool
}

type hooksQuery struct {
	SessionID      *string `json:"sessionId"`
	AgentID        *string `json:"agentId"`
	SessionFromEnv *string `json:"sessionFromEnv"`
	NowOffsetMS    int64   `json:"nowOffsetMs"`
	MaxAgeMS       *int64  `json:"maxAgeMs"`
	Reader         *struct {
		Observations []harness.HookObservation
		Ignored      int
		Reason       *string
	} `json:"reader"`
	Check struct{ Name, Severity, Evidence string } `json:"check"`
}

type hooksTrustCase struct {
	Name             string            `json:"name"`
	Manifest         *string           `json:"manifest"`
	Files            map[string]string `json:"files"`
	Config           *string           `json:"config"`
	Key              *string           `json:"key"`
	CodexHomeFromEnv bool              `json:"codexHomeFromEnv"`
	Expect           struct {
		Severity string
		Evidence *string
		Repair   string
	} `json:"expect"`
}

func hooksSum(data string) string { h := sha256.Sum256([]byte(data)); return hex.EncodeToString(h[:]) }

func hooksLookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { v, ok := env[key]; return v, ok }
}

func hooksWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hooksLoad(t *testing.T) hooksOracle {
	t.Helper()
	raw, err := os.ReadFile("testdata/harness/hooks/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle hooksOracle
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	return oracle
}

// hooksOracleScript is the component script the oracle names; the Go writer names the manifest.
func hooksOracleScript(s string) string {
	return regexp.MustCompile(`components/[a-z0-9-]+/src/cli\.ts`).ReplaceAllString(s, harness.Entrypoint)
}

func hooksAgentKey(agent *string) string {
	if agent == nil {
		return "null"
	}
	quoted, _ := json.Marshal(*agent)
	return string(quoted)
}

func TestHarnessHooksReplayTheOracleReader(t *testing.T) {
	oracle := hooksLoad(t)
	digest := hooksSum(oracle.Manifest)
	for _, c := range oracle.Reader {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			plugin, home := filepath.Join(dir, "plugin"), filepath.Join(dir, "home")
			manifest := filepath.Join(plugin, ".codex-plugin", "plugin.json")
			hooksWrite(t, manifest, oracle.Manifest)
			real, _ := filepath.EvalSymlinks(plugin)
			text := strings.NewReplacer("@@PLUGIN_ROOT@@", real, "@@MANIFEST_DIGEST@@", digest, "@@ENTRY_DIGEST@@", digest)
			where := func(f hooksFile, slot []string) string {
				name := f.Name
				if slot != nil {
					key, _ := json.Marshal([]string{slot[0], slot[1], harness.Entrypoint})
					name = hooksSum(string(key)) + ".json"
				}
				return filepath.Join(home, "crw", "hook-observations", hooksSum(f.Actor.Session), hooksSum(hooksAgentKey(f.Actor.Agent)), name)
			}
			for _, f := range c.Files {
				path := where(f, f.Slot)
				switch {
				case f.Dir:
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				case f.Fifo:
					hooksWrite(t, filepath.Join(path, "..", ".keep"), "")
					if err := syscall.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				case f.SymlinkTo != nil:
					hooksWrite(t, filepath.Join(path, "..", ".keep"), "")
					if err := os.Symlink(where(f, f.SymlinkTo), path); err != nil {
						t.Fatal(err)
					}
				default:
					data := hooksOracleScript(text.Replace(f.Text))
					if f.PadTo > 0 { // a record of exactly the read bound, whatever the length of this directory's path
						data = strings.TrimSuffix(data, "}") + strings.Repeat(" ", f.PadTo-len(data)) + "}"
					}
					hooksWrite(t, path, data)
				}
			}
			switch c.Mutation {
			case "entrypoint", "manifest_extra":
				hooksWrite(t, manifest, oracle.Manifest[:len(oracle.Manifest)-1]+",\"hooks\":[]}")
			case "manifest_missing":
				_ = os.Remove(manifest)
			case "manifest_noversion":
				hooksWrite(t, manifest, `{"name":"x"}`)
			case "manifest_symlink":
				_ = os.Remove(manifest)
				hooksWrite(t, filepath.Join(dir, "real.json"), oracle.Manifest)
				_ = os.Symlink(filepath.Join(dir, "real.json"), manifest)
			case "ancestor_file":
				_ = os.RemoveAll(filepath.Join(home, "crw"))
				hooksWrite(t, filepath.Join(home, "crw"), "not a directory")
			}
			for i, q := range c.Queries {
				now := oracle.T0 + q.NowOffsetMS
				if q.Reader != nil {
					query := harness.ObservationQuery{PluginRoot: plugin, CodexHome: home, SessionID: *q.SessionID, AgentID: q.AgentID, NowMS: now, MaxAgeMS: harness.HookObservationMaxAgeMS}
					if q.MaxAgeMS != nil {
						query.MaxAgeMS = *q.MaxAgeMS
					}
					got := harness.ReadHookObservations(query)
					reason := ""
					if q.Reader.Reason != nil {
						reason = *q.Reader.Reason
					}
					want := q.Reader.Observations
					for j := range want {
						want[j].Entrypoint = harness.Entrypoint
					}
					// The oracle's order is the order to match: every recorded query holds one entrypoint and distinct events.
					if got.Ignored != q.Reader.Ignored || got.Reason != reason || len(got.Observations) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got.Observations, want)) {
						t.Errorf("query %d reader: got %+v, oracle %+v ignored %d reason %q", i, got, want, q.Reader.Ignored, reason)
					}
				}
				options := doctor.HarnessOptions{CodexHome: home, SessionID: q.SessionID, AgentID: q.AgentID, ObservationNow: &now, ObservationMaxAgeMS: q.MaxAgeMS}
				env := map[string]string{}
				if q.SessionFromEnv != nil {
					env["CODEX_THREAD_ID"] = *q.SessionFromEnv
				}
				check := doctor.HarnessHookExecutionCheck(plugin, options, hooksLookup(env), time.UnixMilli(now))
				if check.Name != q.Check.Name || string(check.Severity) != q.Check.Severity || check.Evidence != hooksOracleScript(q.Check.Evidence) || check.Repair != "" {
					t.Errorf("query %d check: got %+v, oracle %+v", i, check, q.Check)
				}
			}
		})
	}
}

func TestHarnessHooksReplayTheOracleTrustCheck(t *testing.T) {
	for _, c := range hooksLoad(t).Trust {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			plugin, home := filepath.Join(dir, "plugin"), filepath.Join(dir, "home")
			if err := os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755); err != nil {
				t.Fatal(err)
			}
			if c.Manifest != nil {
				hooksWrite(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), *c.Manifest)
			}
			for name, data := range c.Files {
				hooksWrite(t, filepath.Join(plugin, name), data)
			}
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.Config != nil {
				hooksWrite(t, filepath.Join(home, "config.toml"), *c.Config)
			}
			options, env := doctor.HarnessOptions{CodexHome: home}, map[string]string{}
			if c.CodexHomeFromEnv {
				options.CodexHome, env["CODEX_HOME"] = "", home
			}
			if c.Key != nil {
				options.PluginKey = *c.Key
			}
			got := doctor.HarnessHookTrustCheck(plugin, options, hooksLookup(env))
			names := strings.NewReplacer("@@CODEX_HOME@@", home, "cxc hooks retrust", "crw doctor retrust")
			if got.Name != "hook-trust" || string(got.Severity) != c.Expect.Severity || got.Repair != names.Replace(c.Expect.Repair) {
				t.Errorf("got %+v, oracle %+v", got, c.Expect)
			}
			if c.Expect.Evidence == nil && got.Evidence == "" || c.Expect.Evidence != nil && got.Evidence != names.Replace(*c.Expect.Evidence) {
				t.Errorf("evidence %q, oracle %v", got.Evidence, c.Expect.Evidence)
			}
		})
	}
}

// hooksWriterHome is a CODEX_HOME and a plugin root whose manifest the Go writer can record against.
func hooksWriterHome(t *testing.T) (env map[string]string, home, plugin string) {
	t.Helper()
	dir := t.TempDir()
	home, plugin = filepath.Join(dir, "codex"), filepath.Join(dir, "plugin")
	hooksWrite(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"crw","version":"1.2.3"}`)
	return map[string]string{"CODEX_HOME": home, "PLUGIN_ROOT": plugin, "HOME": dir}, home, plugin
}

func TestHarnessHookExecutionCheckReadsWhatTheWriterWrites(t *testing.T) {
	env, home, plugin := hooksWriterHome(t)
	session, child := "s1", "child-a"
	present := regexp.MustCompile(`^session=s1 actor=root: 1 current invocation\(s\): cxc-ops/session-start \(\.codex-plugin/plugin\.json, \d{4}-\d\d-\d\dT[\d:.]{12}Z\); 0 ignored record\(s\)\. Declaration coverage and handler results remain unknown\. Same-user writable/replayable diagnostics, not host attestations or proof of enforcement\.$`)
	run := func(options doctor.HarnessOptions, env map[string]string, now time.Time) doctor.HarnessCheck {
		return doctor.HarnessHookExecutionCheck(plugin, options, hooksLookup(env), now)
	}
	// Nothing recorded yet: the check warns and says why.
	if got := run(doctor.HarnessOptions{SessionID: &session}, env, time.Now()); got.Severity != doctor.HarnessWarn || !strings.Contains(got.Evidence, "unverified (no invocation records)") {
		t.Errorf("absent: %+v", got)
	}
	// Only a child recorded: the root is still unverified.
	if !harness.RecordInvocation(`{"session_id":"s1","agent_id":"child-a","agent_type":"executor"}`, "cxc-ops", "session-start", hooksLookup(env)) {
		t.Fatal("child not recorded")
	}
	if got := run(doctor.HarnessOptions{SessionID: &session}, env, time.Now()); got.Severity != doctor.HarnessWarn {
		t.Errorf("partial: %+v", got)
	}
	if got := run(doctor.HarnessOptions{SessionID: &session, AgentID: &child}, env, time.Now()); got.Severity != doctor.HarnessPass || !strings.Contains(got.Evidence, "actor=child-a") {
		t.Errorf("child: %+v", got)
	}
	if !harness.RecordInvocation(`{"session_id":"s1"}`, "cxc-ops", "session-start", hooksLookup(env)) {
		t.Fatal("root not recorded")
	}
	// CODEX_HOME and the session come from the environment when the options leave them out, the clock from the argument.
	env["CODEX_THREAD_ID"] = "s1"
	if got := run(doctor.HarnessOptions{}, env, time.Now()); got.Severity != doctor.HarnessPass || !present.MatchString(got.Evidence) {
		t.Errorf("present: %+v", got)
	}
	if got := run(doctor.HarnessOptions{}, env, time.Now().Add(25*time.Hour)); got.Severity != doctor.HarnessWarn || !strings.Contains(got.Evidence, "unverified (no matching fresh evidence); 1 ignored record(s)") {
		t.Errorf("stale: %+v", got)
	}
	// Without CODEX_HOME the store is under HOME/.codex, which holds nothing here.
	delete(env, "CODEX_HOME")
	if got := run(doctor.HarnessOptions{}, env, time.Now()); got.Severity != doctor.HarnessWarn || !strings.Contains(got.Evidence, "no invocation records") {
		t.Errorf("home default: %+v", got)
	}
	if err := os.Rename(home, filepath.Join(env["HOME"], ".codex")); err != nil {
		t.Fatal(err)
	}
	if got := run(doctor.HarnessOptions{}, env, time.Now()); got.Severity != doctor.HarnessPass {
		t.Errorf("home default with records: %+v", got)
	}
}

func TestHarnessHookTrustCheckRefusesAManifestLinkedOutOfThePlugin(t *testing.T) {
	dir := t.TempDir()
	plugin, home := filepath.Join(dir, "plugin"), filepath.Join(dir, "home")
	hooksWrite(t, filepath.Join(dir, "outside.json"), `{"name":"fixture","hooks":[]}`)
	if err := os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "outside.json"), filepath.Join(plugin, ".codex-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
	got := doctor.HarnessHookTrustCheck(plugin, doctor.HarnessOptions{CodexHome: home}, hooksLookup(nil))
	if got.Severity != doctor.HarnessFail || !strings.Contains(got.Evidence, "symlink escapes plugin root") {
		t.Errorf("%+v", got)
	}
}
