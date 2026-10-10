package cli

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1108 (B2-01): a terminal mutation that runs inside a native Codex session (CODEX_THREAD_ID set)
// may change only that session. The B2 reproduction ran native 0190cafe-…-279 and changed foreign
// 0190cafe-…-280 IDLE->P with exit 0; the guard refuses it before any lock, state or ledger write.
// A terminal without the native environment keeps its explicit id, the reserved key included, and a
// native database that cannot be read is not a reason to refuse an id that equals CODEX_THREAD_ID.

const (
	guardNativeID  = "0190cafe-0279-7000-8000-000000000279"
	guardForeignID = "0190cafe-0279-7000-8000-000000000280"
)

// guardNativeDB writes state_5.sqlite under root/codex with one root (cli) thread at cwd.
func guardNativeDB(t *testing.T, root, id, cwd string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{"CREATE TABLE threads (id TEXT, cwd TEXT, archived INTEGER, source TEXT)", "INSERT INTO threads VALUES ('" + id + "', '" + cwd + "', 0, 'cli')"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrchestrateMutationRefusesAnotherNativeSession(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	guardNativeDB(t, root, guardNativeID, cwd)
	statusFile(t, state.StatePath(cwd, guardNativeID), `{"phase":"IDLE","sessionId":"`+guardNativeID+`"}`)
	statusFile(t, state.StatePath(cwd, guardForeignID), `{"phase":"IDLE","sessionId":"`+guardForeignID+`"}`)
	native := statusEnv(map[string]string{"CODEX_THREAD_ID": guardNativeID, "CODEX_HOME": filepath.Join(root, "codex")})
	for _, argv := range [][]string{
		{"P", "--session", guardForeignID},
		{"reset", "--session", guardForeignID},
		{"A", "--session", guardForeignID, "--attest", `{"from":"P","to":"A","did":"x"}`},
		{"A", "--session", guardForeignID, "--attest", "not json"}, // the attestation error would read the foreign phase
		{"P", "--session", "cli"},                                  // SESSION-IDENTITY-01: never substitute cli inside a native session
	} {
		before := statusTree(t, cwd)
		got, err := RunOrchestrateRead(ParseOrchestrateCliArgs(argv, cwd), ReadEnv{Native: native})
		if err != nil || got.Result == nil || got.Result.Code != 1 {
			t.Fatalf("%v: a foreign id under a native session was delegated: %+v %v", argv, got, err)
		}
		for _, want := range []string{"orchestrate " + argv[0] + ": ", "--session '" + argv[2] + "'", guardNativeID, "SESSION-IDENTITY-01", "Nothing was written."} {
			if !strings.Contains(got.Result.Output, want) {
				t.Fatalf("%v: refusal %q lacks %q", argv, got.Result.Output, want)
			}
		}
		if strings.Contains(got.Result.Output, "current=") {
			t.Fatalf("%v: the refusal rendered the foreign session's phase: %q", argv, got.Result.Output)
		}
		if !reflect.DeepEqual(before, statusTree(t, cwd)) {
			t.Fatalf("%v: the refusal wrote to the workspace", argv)
		}
	}
	// The verified database is named in the refusal; an unreadable one is not, but the id still decides.
	got, _ := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"P", "--session", guardForeignID}, cwd), ReadEnv{Native: native})
	if got.Result == nil || !strings.Contains(got.Result.Output, "confirmed by the native thread database") {
		t.Fatalf("verified refusal: %+v", got.Result)
	}
	noDB := statusEnv(map[string]string{"CODEX_THREAD_ID": guardNativeID, "CODEX_HOME": filepath.Join(root, "missing")})
	got, err := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"P", "--session", guardForeignID}, cwd), ReadEnv{Native: noDB})
	if err != nil || got.Result == nil || got.Result.Code != 1 || strings.Contains(got.Result.Output, "confirmed by") {
		t.Fatalf("a foreign id with an unreadable native database: %+v %v", got.Result, err)
	}
}

func TestOrchestrateMutationKeepsTheOwnNativeAndTheStandaloneTerminal(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	guardNativeDB(t, root, guardNativeID, cwd)
	statusFile(t, state.StatePath(cwd, guardNativeID), `{"phase":"IDLE","sessionId":"`+guardNativeID+`"}`)
	statusFile(t, state.StatePath(cwd, guardForeignID), `{"phase":"IDLE","sessionId":"`+guardForeignID+`"}`)
	for _, tc := range []struct {
		name    string
		env     map[string]string
		session string
	}{
		{"own id, verified", map[string]string{"CODEX_THREAD_ID": guardNativeID, "CODEX_HOME": filepath.Join(root, "codex")}, guardNativeID},
		// a database lookup failure is not widened into a refusal: the id equals CODEX_THREAD_ID
		{"own id, no native database", map[string]string{"CODEX_THREAD_ID": guardNativeID, "CODEX_HOME": filepath.Join(root, "missing")}, guardNativeID},
		{"no native environment, explicit id", map[string]string{}, guardForeignID},
		{"no native environment, terminal key", map[string]string{}, "cli"},
		{"empty CODEX_THREAD_ID is no native environment", map[string]string{"CODEX_THREAD_ID": ""}, guardForeignID},
	} {
		got, err := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"P", "--session", tc.session}, cwd), ReadEnv{Native: statusEnv(tc.env)})
		if err != nil || got.Result != nil || got.SessionID != tc.session {
			t.Fatalf("%s: %+v %v", tc.name, got.Result, err)
		}
	}
	// status stays a read: an explicit foreign id is shown, not refused
	got, err := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"status", "--session", guardForeignID}, cwd), ReadEnv{Native: statusEnv(map[string]string{"CODEX_THREAD_ID": guardNativeID, "CODEX_HOME": filepath.Join(root, "codex")})})
	if err != nil || got.Result == nil || got.Result.Code != 0 || !strings.Contains(got.Result.Output, "session="+guardForeignID) {
		t.Fatalf("explicit status of another session: %+v %v", got.Result, err)
	}
}
