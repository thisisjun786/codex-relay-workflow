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

// handshakeAnswers is the inert answer each registered shim gives a start-up handshake, as it stood at
// 1633ddc2a, as the output's JSON value. Seven shims answer null, the answer the handshake is specified
// to get. The memorygate, shellwrite, spawn and worktreedel shims answer their own inert non-null values
// for a non-object input (CRW-932 keeps those answers byte-identical). A new registered shim has no
// entry here and fails the test until it is added.
var handshakeAnswers = map[string]string{
	"echo":        `null`,
	"doctor":      `null`,
	"goalplan":    `null`,
	"pyjson":      `null`,
	"state":       `null`,
	"memorygate":  `""`,
	"shellwrite":  `[]`,
	"spawn":       `"the input is outside the target's grammar"`,
	"worktreedel": `{"decision":"allow","reason":""}`,
}

// startupImportTargets are the registered shims that import their original module at their top level,
// before they read any request. With the original root empty that import fails, so these shims give no
// handshake answer and their empty-oracle-root case is skipped. The skip is an open item of CRW-932
// (docs/port-cxc/known-defects/CRW-932.md): answering before the import needs a change to each of these
// five shims, which this issue's scope does not include. The doctor shim answers before its import and
// is covered by the same case.
var startupImportTargets = map[string]bool{
	"state":       true,
	"goalplan":    true,
	"memorygate":  true,
	"shellwrite":  true,
	"worktreedel": true,
}

// A start-up handshake is one request with a null input and a root (CRW-854). Each registered shim
// must answer it inertly, without building a path from the root, mirroring or reading a document, or
// importing its original module: a shim that ran the case would create .codexclaw/... in the worker's
// own working directory on every worker start, the pyjson shim would spawn python3, and the doctor
// shim would import cxc-ops/dist/doctor.js under the host's inherited environment (CRW-932).
func TestShimsAnswerTheStartupHandshakeInertly(t *testing.T) {
	requireNode(t)
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range registeredShimTargets() {
		t.Run(name, func(t *testing.T) {
			requireOracleModule(t, name)
			want, ok := handshakeAnswers[name]
			if !ok {
				t.Fatalf("the registered shim %s has no expected handshake answer in handshakeAnswers", name)
			}
			// The pool's own start-up request carries an empty root.
			t.Run("empty root", func(t *testing.T) {
				checkStartupHandshake(t, root, name, "", DefaultOracleRoot, want)
			})
			// The same request with the root set to an empty directory: the shim must still answer
			// inertly and must leave that directory empty, so it did not run a case under it.
			t.Run("empty directory root", func(t *testing.T) {
				checkStartupHandshake(t, root, name, t.TempDir(), DefaultOracleRoot, want)
			})
			// The same request with the original root set to an empty directory: a shim that imports
			// its original module before it answers fails here, so this proves the answer came first.
			t.Run("empty oracle root", func(t *testing.T) {
				if startupImportTargets[name] {
					t.Skip("the shim imports its original module at its top level, before it reads a request; an empty original root fails that import (CRW-932 open item, known-defects/CRW-932.md)")
				}
				checkStartupHandshake(t, root, name, "", t.TempDir(), want)
			})
		})
	}
}

// checkStartupHandshake sends one start-up handshake for the named shim with the given request root and
// original (oracle) root, checks the answer is the want output (a JSON value) for request 1 with no
// error, and that nothing was written under the worker's working directory or the request root.
func checkStartupHandshake(t *testing.T, root, name, requestRoot, oracleRoot, want string) {
	t.Helper()
	dir := t.TempDir()
	// The shim's homes go in a sibling directory, not in dir: dir is the worker's working
	// directory, and this test proves the handshake writes nothing there.
	envDir := t.TempDir()
	// c3 requires every test that can reach a shim to point HOME, CODEX_HOME, CRW_HOME and TMPDIR
	// at a temporary directory before it starts one: the shim imports the oracle at its top level,
	// and an inherited real home would be read by that import (CRW-708 generation 5, d6 of the
	// pre-merge evaluation).
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

	request, err := json.Marshal(map[string]any{"id": 1, "input": nil, "root": requestRoot})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, string(request)+"\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the shim did not answer the handshake: %v", err)
	}
	// The answer must be the inert one: the reply to request 1 carrying the shim's own inert output
	// (null for the seven shims the handshake is specified for). A shim that ran the case still answers
	// (an error envelope or a case answer) and may leave no file behind, so checking only for a reply
	// and an empty directory would miss it: the pyjson shim would spawn python3 on every worker start
	// and still pass.
	var reply struct {
		ID     int             `json:"id"`
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("the handshake answer %q is not JSON: %v", line, err)
	}
	if reply.ID != 1 || len(reply.Error) != 0 {
		t.Fatalf("the handshake answer %q is not an inert answer to request 1 without an error", line)
	}
	var got, expected bytes.Buffer
	if err := json.Compact(&got, reply.Output); err != nil {
		t.Fatalf("the handshake output %q is not JSON: %v", reply.Output, err)
	}
	if err := json.Compact(&expected, []byte(want)); err != nil {
		t.Fatalf("the expected handshake output %q is not JSON: %v", want, err)
	}
	if got.String() != expected.String() {
		t.Fatalf("the handshake answer %q does not carry the inert output %s", line, want)
	}
	// The reply is discarded by the pool, so only its presence matters. Wait for the process
	// to exit, then prove the handshake wrote nothing under the worker's working directory or
	// under the request root.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdout); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the shim did not exit after its stdin closed")
	}
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
