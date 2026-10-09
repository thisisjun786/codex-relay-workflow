//go:build dev

package cxcfuzz

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// shimTargets are the three targets whose oracle shim this issue owns. The echo, shellwrite and
// memorygate shims belong to other issues.
func shimTargets() []string { return []string{"pyjson", "state", "goalplan"} }

// registeredShimTargets is every target the registry runs through a worker shim, read from the
// registry rather than a list here, so a target added there is covered by the handshake test
// without an edit to this file (CRW-932).
func registeredShimTargets() []string {
	var names []string
	for _, target := range registry() {
		if target.Oracle.Shim != "" {
			names = append(names, target.Name)
		}
	}
	sort.Strings(names)
	return names
}

// handshakeAnswers is the exact reply line (without its newline) each registered shim gives a start-up
// handshake `{"id":1,"input":null,"root":...}`. The nine lines are the bytes the shims write: the five
// that answer null, and the memorygate, shellwrite, spawn and worktreedel shims' own inert non-null values
// for a non-object input. The eight shims other than doctor are pinned as they stood at 1633ddc2a, taken
// from running those unchanged shims there (CRW-932 keeps those answers byte-identical). The test compares
// the raw line, so a change to the key order, the spacing or an added field is noticed. A new registered
// shim has no entry here and fails the test until it is added.
var handshakeAnswers = map[string]string{
	"echo":        `{"id":1,"output":null}`,
	"doctor":      `{"id":1,"output":null}`,
	"goalplan":    `{"id":1,"output":null}`,
	"pyjson":      `{"id":1,"output":null}`,
	"state":       `{"id":1,"output":null}`,
	"memorygate":  `{"id":1,"output":""}`,
	"shellwrite":  `{"id":1,"output":[]}`,
	"spawn":       `{"id":1,"output":"the input is outside the target's grammar"}`,
	"worktreedel": `{"id":1,"output":{"decision":"allow","reason":""}}`,
}

// handshakeOutput is the output value of the pinned reply line for the named shim.
func handshakeOutput(t *testing.T, name string) string {
	t.Helper()
	var reply struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal([]byte(handshakeAnswers[name]), &reply); err != nil {
		t.Fatalf("the pinned handshake answer for %s is not JSON: %v", name, err)
	}
	return string(reply.Output)
}

// startupLoadingShims are the shims that import their original module at their top level, when the
// worker starts and before it listens, so the pool's readiness probe waits for the module. With an
// original root that holds no module they exit non-zero before answering anything; that is the behaviour
// before CRW-932 and it stays (a worker whose original module cannot load fails at start). The set is
// pinned so a change to it is noticed: a shim that starts loading lazily, or a new top-level loader,
// fails the empty-oracle-root case until the set is updated.
var startupLoadingShims = map[string]bool{
	"goalplan":    true,
	"memorygate":  true,
	"shellwrite":  true,
	"state":       true,
	"worktreedel": true,
}

// A start-up handshake is one request with a null input and a root (CRW-854). Each registered shim
// must answer it inertly, without building a path from the root or mirroring or reading a document: a
// shim that ran the case would create .codexclaw/... in the worker's own working directory on every
// worker start, and the pyjson shim would spawn python3. The doctor shim must also not import
// cxc-ops/dist/doctor.js for it, under the host's inherited environment (CRW-932).
//
// Three cases per shim. The root is empty (the pool's own handshake), the root is an empty directory
// that must stay empty, and the original root (ORACLE_ROOT) is an empty directory, so no original module
// can load. In that last case the shims that load their original module when they start
// (startupLoadingShims) keep their existing behaviour: the worker exits non-zero before it answers, with
// the module-not-found error. Every other shim must still give its inert answer and leave the working
// directory empty. TestDoctorShimImportsNothingForTheHandshake shows by a recorded import attempt that
// the doctor shim does not import for the handshake.
func TestShimsAnswerTheStartupHandshakeInertly(t *testing.T) {
	requireNode(t)
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range registeredShimTargets() {
		t.Run(name, func(t *testing.T) {
			want, ok := handshakeAnswers[name]
			if !ok {
				t.Fatalf("the registered shim %s has no expected handshake answer in handshakeAnswers", name)
			}
			// The pool's own start-up request carries an empty root.
			t.Run("empty root", func(t *testing.T) {
				requireOracleModule(t, name)
				checkStartupHandshake(t, root, name, "", DefaultOracleRoot, want)
			})
			// The same request with the root set to an empty directory: the shim must still answer
			// inertly and must leave that directory empty, so it did not run a case under it.
			t.Run("empty directory root", func(t *testing.T) {
				requireOracleModule(t, name)
				checkStartupHandshake(t, root, name, t.TempDir(), DefaultOracleRoot, want)
			})
			// The same request with the original root set to an empty directory. This case does not
			// read the host's original tree, so it is not skipped when that tree is absent.
			t.Run("empty oracle root", func(t *testing.T) {
				if startupLoadingShims[name] {
					checkStartupLoaderFailsBeforeAnswering(t, root, name, t.TempDir())
					return
				}
				checkStartupHandshake(t, root, name, "", t.TempDir(), want)
			})
		})
	}
}

// The memorygate and shellwrite shims answer a null input with the value the Go side gives the same
// input, so a null input is a case both sides agree on. Their answers stay as they were (CRW-932: "the
// other shims' existing answers stay"): answering null instead would make the oracle disagree with the
// port on that input. The spawn shim's refusal is pinned by TestSpawnShimAnswersTheHandshakeInertly
// (CRW-938); the Go side of spawn and worktreedel reports an error for a non-object input rather than an
// answer, so their shim answers are not comparable this way and are pinned by handshakeAnswers alone.
func TestNonNullHandshakeAnswersAreThePortsAnswerToNullInput(t *testing.T) {
	for _, name := range []string{"memorygate", "shellwrite"} {
		t.Run(name, func(t *testing.T) {
			target, ok := Lookup(name)
			if !ok {
				t.Fatalf("no registered target %s", name)
			}
			rootDir := t.TempDir()
			got, err := target.Go(nil, Env{Root: rootDir, Home: filepath.Join(rootDir, "home"), CodexHome: filepath.Join(rootDir, "codex-home"), CrwHome: filepath.Join(rootDir, "crw-home"), TmpDir: filepath.Join(rootDir, "tmp")})
			if err != nil {
				t.Fatalf("the Go side failed a null input: %v", err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(canonical(got))); err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := json.Compact(&want, []byte(handshakeOutput(t, name))); err != nil {
				t.Fatal(err)
			}
			if compact.String() != want.String() {
				t.Fatalf("the Go side answers a null input %s; handshakeAnswers pins the shim's %s", compact.String(), want.String())
			}
		})
	}
}

// The doctor shim imports its original module per request. For the handshake it must not import at all:
// a fake original tree whose module records its evaluation shows the attempt. The handshake leaves no
// record; a case input (the control) does, so the record is a real signal and not a path that never
// fires (CRW-932; the empty-original-root case alone cannot tell, because a shim that tried the import
// and caught its failure answers the same).
func TestDoctorShimImportsNothingForTheHandshake(t *testing.T) {
	requireNode(t)
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	module := filepath.Join(fake, "cxc-ops", "dist")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	// The module takes a moment to evaluate (a top-level await), as a real import of a large tree
	// does, so the control case still has the import in flight when a caller's stdin closes.
	source := "import { appendFileSync } from \"node:fs\";\nawait new Promise((resolve) => setTimeout(resolve, 300));\nappendFileSync(process.env.CRW932_IMPORT_RECORD, \"imported\\n\");\n"
	if err := os.WriteFile(filepath.Join(module, "doctor.js"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	record := func() string { return filepath.Join(t.TempDir(), "import-record") }
	// Every input that is not an object is answered before the import: the start-up handshake's null
	// and the scalars and array a malformed case could carry.
	for _, input := range []string{`null`, `"x"`, `0`, `true`, `[]`} {
		nonObjectRecord := record()
		line := runShim(t, root, "doctor", fake, []string{"CRW932_IMPORT_RECORD=" + nonObjectRecord}, `{"id":1,"input":`+input+`,"root":""}`)
		if strings.TrimSpace(line) != `{"id":1,"output":null}` {
			t.Fatalf("the doctor shim answered the non-object input %s with %q, want {\"id\":1,\"output\":null}", input, line)
		}
		if _, err := os.Stat(nonObjectRecord); err == nil {
			t.Fatalf("the doctor shim imported its original module for the non-object input %s", input)
		}
	}
	controlRecord := record()
	runShim(t, root, "doctor", fake, []string{"CRW932_IMPORT_RECORD=" + controlRecord}, `{"id":1,"input":{},"root":""}`)
	if _, err := os.Stat(controlRecord); err != nil {
		t.Fatalf("the control case left no import record, so the record cannot show a handshake import: %v", err)
	}
}

// runShim starts the named shim in an empty working directory with its homes and TMPDIR in a temporary
// directory outside it and ORACLE_ROOT set to oracleRoot (plus extraEnv), sends it one request line,
// reads the one reply line, and only then closes its stdin and waits for the shim to exit. The pool
// keeps a worker's stdin open while it works, and the doctor shim exits as soon as its stdin ends, so
// closing it before the reply would end an in-flight import and lose the reply (CRW-932).
func runShim(t *testing.T, root, name, oracleRoot string, extraEnv []string, request string) string {
	line, _ := runShimIn(t, root, name, oracleRoot, extraEnv, request)
	return line
}

// runShimIn is runShim that also returns the shim's working directory, so a caller can check nothing was
// written there.
func runShimIn(t *testing.T, root, name, oracleRoot string, extraEnv []string, request string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	// The shim's homes go in a sibling directory, not in dir: dir is the worker's working
	// directory, and the handshake test proves the handshake writes nothing there.
	envDir := t.TempDir()
	// c3 requires every test that can reach a shim to point HOME, CODEX_HOME, CRW_HOME and TMPDIR
	// at a temporary directory before it starts one: the shim may import the oracle at its top level,
	// and an inherited real home would be read by that import (CRW-708 generation 5, d6 of the
	// pre-merge evaluation).
	env := append(os.Environ(),
		"HOME="+filepath.Join(envDir, "home"),
		"CODEX_HOME="+filepath.Join(envDir, "codex-home"),
		"CRW_HOME="+filepath.Join(envDir, "crw-home"),
		"TMPDIR="+filepath.Join(envDir, "tmp"),
		"ORACLE_ROOT="+oracleRoot,
	)
	env = append(env, extraEnv...)
	for _, sub := range []string{"home", "codex-home", "crw-home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(envDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shim := filepath.Join(root, "internal", "dev", "cxcfuzz", "testdata", name, "shim.mjs")
	cmd := exec.Command("node", shim)
	cmd.Dir = dir
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	if _, err := io.WriteString(stdin, request+"\n"); err != nil {
		t.Fatal(err)
	}
	// A shim that never answers fails the test at the deadline instead of hanging it.
	timer := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("the shim did not answer: %v", err)
	}
	// The reply is in, so closing stdin now cannot cut an answer short. Wait for the process to exit
	// before the caller looks at what it wrote.
	_ = stdin.Close()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the shim did not exit after its stdin closed")
	}
	return line, dir
}

// checkStartupHandshake sends one start-up handshake for the named shim with the given request root and
// original (oracle) root, checks the raw reply line (without its newline) is exactly want, and that
// nothing was written under the worker's working directory or the request root.
func checkStartupHandshake(t *testing.T, root, name, requestRoot, oracleRoot, want string) {
	t.Helper()
	request, err := json.Marshal(map[string]any{"id": 1, "input": nil, "root": requestRoot})
	if err != nil {
		t.Fatal(err)
	}
	line, dir := runShimIn(t, root, name, oracleRoot, nil, string(request))
	// The answer must be the pinned one byte for byte. A shim that ran the case still answers (an error
	// envelope or a case answer) and may leave no file behind, so checking only for a reply and an empty
	// directory would miss it: the pyjson shim would spawn python3 on every worker start and still pass.
	if got := strings.TrimSuffix(line, "\n"); got != want {
		t.Fatalf("the handshake answer is %q, want exactly %q", got, want)
	}
	// The handshake wrote nothing under the worker's working directory or under the request root.
	for _, where := range []string{dir, requestRoot} {
		if where == "" {
			continue
		}
		if names := dirEntryNames(t, where); len(names) > 0 {
			t.Fatalf("the handshake wrote under %s: %v", where, names)
		}
	}
}

// dirEntryNames is the sorted names of the entries in dir.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// checkStartupLoaderFailsBeforeAnswering starts a start-up-loading shim with an original root that holds
// no module and sends the start-up handshake. The worker must exit non-zero without writing an answer,
// with the module-not-found error on its stderr, and must leave its working directory empty.
func checkStartupLoaderFailsBeforeAnswering(t *testing.T, root, name, oracleRoot string) {
	t.Helper()
	dir := t.TempDir()
	envDir := t.TempDir()
	env := append(os.Environ(),
		"HOME="+filepath.Join(envDir, "home"),
		"CODEX_HOME="+filepath.Join(envDir, "codex-home"),
		"CRW_HOME="+filepath.Join(envDir, "crw-home"),
		"TMPDIR="+filepath.Join(envDir, "tmp"),
		"ORACLE_ROOT="+oracleRoot,
	)
	for _, sub := range []string{"home", "codex-home", "crw-home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(envDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("node", filepath.Join(root, "internal", "dev", "cxcfuzz", "testdata", name, "shim.mjs"))
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader("{\"id\":1,\"input\":null,\"root\":\"\"}\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatal("the shim neither answered nor exited with an empty original root")
	}
	if waitErr == nil {
		t.Fatalf("the shim exited 0 with an empty original root; stdout %q", stdout.String())
	}
	if _, isExit := waitErr.(*exec.ExitError); !isExit {
		t.Fatalf("the shim did not run to an exit status: %v", waitErr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("the shim answered %q before failing to load its original module", stdout.String())
	}
	if !strings.Contains(stderr.String(), "ERR_MODULE_NOT_FOUND") {
		t.Fatalf("the shim failed, but not with the module-not-found error: %s", stderr.String())
	}
	if names := dirEntryNames(t, dir); len(names) > 0 {
		t.Fatalf("the failed start wrote under the working directory: %v", names)
	}
}
