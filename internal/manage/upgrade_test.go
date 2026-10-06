package manage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// upgradeEnv is one run's environment: temporary homes, a fake crw and gh, a release directory
// holding one verified archive, and a temporary relay store.
type upgradeEnv struct {
	t       *testing.T
	home    string
	codex   string
	state   string
	release string
	calls   string
	ghCalls string
}

type upgradeHarnessOptions struct {
	version      string
	openAttempts int
	gh           map[string]upgradeGhAnswer
	pointer      bool
	installExit  int
	mutateConfig bool
	stopExit     int
}

type upgradeGhAnswer struct {
	Body string
	Exit int
}

func upgradeHarness(t *testing.T, opts upgradeHarnessOptions) *upgradeEnv {
	t.Helper()
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	state := filepath.Join(home, "relay-state")
	for _, dir := range []string{codex, state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("XDG_STATE_HOME", "")
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte("model = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &upgradeEnv{t: t, home: home, codex: codex, state: state, release: t.TempDir(),
		calls: filepath.Join(home, "crw-calls.jsonl"), ghCalls: filepath.Join(home, "gh-calls.jsonl")}
	h.writeArchive(opts)
	h.writeStore(opts.openAttempts)
	h.writeFakes(opts)
	h.installPointer(opts.pointer)
	return h
}

func (h *upgradeEnv) writeArchive(opts upgradeHarnessOptions) {
	h.t.Helper()
	body := upgradeTarGz(h.t, map[string]string{"crw": fakeCRWScript(opts.version, opts.installExit, opts.mutateConfig)})
	name := "crw_0.4.0_linux_amd64.tar.gz"
	if err := os.WriteFile(filepath.Join(h.release, name), body, 0o600); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	if err := os.WriteFile(filepath.Join(h.release, upgradeSumsName), []byte(sums), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// fakeCRWScript is the extracted archive's crw: it answers --version and ends install with the
// given status, optionally having changed the configuration first.
func fakeCRWScript(version string, installExit int, mutateConfig bool) string {
	mutate := ""
	if mutateConfig {
		mutate = "printf 'changed\\n' >> \"$CODEX_HOME/config.toml\"\n"
	}
	return "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + version + "'; exit 0; fi\n" +
		"if [ \"$1\" = \"install\" ]; then " + mutate + "exit " + fmt.Sprint(installExit) + "; fi\n" +
		"exit 0\n"
}

func upgradeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (h *upgradeEnv) writeStore(openAttempts int) {
	h.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.state, "relay.sqlite3"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE attempts (request_id TEXT, internal_state TEXT)"); err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i < 3+openAttempts; i++ {
		state := "settled"
		if i >= 3 {
			state = "in_flight"
		}
		if _, err := db.Exec("INSERT INTO attempts VALUES (?, ?)", fmt.Sprintf("row-%d", i), state); err != nil {
			h.t.Fatal(err)
		}
	}
}

// writeFakes places the fake crw and gh on PATH: crw records its arguments and answers doctor,
// --version and service status; gh answers the API paths asked.
func (h *upgradeEnv) writeFakes(opts upgradeHarnessOptions) {
	h.t.Helper()
	bin := filepath.Join(h.home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		h.t.Fatal(err)
	}
	// Branches match the whole argument list: the relay is called as
	// "codex-session-relay --state S --socket K service status", flags first.
	stop := ""
	if opts.stopExit != 0 {
		stop = "*\"service stop\"*) exit " + fmt.Sprint(opts.stopExit) + ";;\n"
	}
	crw := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + coreShellQuote(h.calls) + "\n" +
		"case \"$*\" in\n" + stop +
		"*doctor*) printf '%s\\n' " + coreShellQuote("{\"stateSelection\":{\"path\":\""+h.state+"\"}}") + "; exit 0;;\n" +
		"*\"--version\"*) printf '%s\\n' " + coreShellQuote("v0.4.0-4633-geb2567df7") + "; exit 0;;\n" +
		"*\"service status\"*) printf '%s\\n' " + coreShellQuote("{\"running\":true,\"launchPolicy\":{\"matchesRunning\":\"same\"}}") + "; exit 0;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "crw"), []byte(crw), 0o700); err != nil {
		h.t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + coreShellQuote(h.ghCalls) + "\n")
	body.WriteString("case \"$2\" in\n")
	for path, answer := range opts.gh {
		body.WriteString(coreShellQuote(path) + ") printf '%s\\n' " + coreShellQuote(answer.Body) + "; exit " + fmt.Sprint(answer.Exit) + ";;\n")
	}
	body.WriteString("*) exit 1;;\nesac\n")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body.String()), 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installPointer places the runtime pointer, with the fakes in its bin so the snapshot and stop
// steps call them too.
func (h *upgradeEnv) installPointer(want bool) {
	h.t.Helper()
	if !want {
		return
	}
	target := filepath.Join(h.home, ".local", "share", "crw-runtime", "bin-0.4.0-aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(target, "bin"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(h.home, "bin", "crw"))
	if err != nil {
		h.t.Fatal(err)
	}
	for _, name := range []string{"crw", "codex-session-relay"} {
		if err := os.WriteFile(filepath.Join(target, "bin", name), body, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(h.home, ".local", "share", "crw-runtime", "current")); err != nil {
		h.t.Fatal(err)
	}
}

func (h *upgradeEnv) run(args ...string) int {
	h.t.Helper()
	old := upgradeConfig
	upgradeConfig = func(e *Env) *Config {
		cfg := coreDefaults(e)
		cfg.Repository = "owner/repo"
		cfg.StateDir = filepath.Join(h.home, "manage-state")
		cfg.Relay.State = h.state
		cfg.Relay.Socket = filepath.Join(h.codex, "app-server-control.sock")
		return cfg
	}
	h.t.Cleanup(func() { upgradeConfig = old })
	var out, errOut strings.Builder
	return Run(context.Background(), append([]string{"runtime-upgrade"}, args...), strings.NewReader(""), &out, &errOut)
}

func upgradeCallLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func (h *upgradeEnv) crwCalls() []string    { return upgradeCallLines(h.t, h.calls) }
func (h *upgradeEnv) ghCallLines() []string { return upgradeCallLines(h.t, h.ghCalls) }

func (h *upgradeEnv) recordOf(t *testing.T) upgradeRecord {
	t.Helper()
	root := filepath.Join(h.home, "manage-state", "upgrades")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the record directory holds %d entries, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(root, entries[0].Name(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record upgradeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

const upgradeGoodCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func upgradeGhPaths(commit string) map[string]upgradeGhAnswer {
	return map[string]upgradeGhAnswer{
		"repos/owner/repo/commits/eb2567df7":                 {Body: "{\"sha\":\"" + commit + "\"}"},
		"repos/owner/repo/commits/" + commit + "/check-runs": {Body: "{\"check_runs\":[{\"name\":\"dev-gate\",\"conclusion\":\"success\"}]}"},
	}
}
