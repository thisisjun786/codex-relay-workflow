package delivery

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// USL-23: an uncertain merge-turn grant is read as status reads it. The merge turn itself
// (request, promotion, grant, acknowledgement, regrant) is mergeturn.py's, todo 26's to port, so
// testdata/grantstage.py stages and answers it in Python on a store in the Go test's tree; the
// part this property is about - reconciling the held grant notice and reading its status - runs
// in Go over that same file (internal/relay/mergeturn is the todo-21 subset of the grant rule).
// Every value the Python test asserts is compared with the Go value at the same assertion.

type grantStage struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	staged struct {
		Waiting, Turn, Event, Store string
		Record                      map[string]any
		Receipt                     map[string]any
		Sends                       [][]any
		Now                         float64
		Threads                     []string
	}
}

func startGrantStage(t *testing.T, tree string) *grantStage {
	root := repoRoot(t)
	script, _ := filepath.Abs("testdata/grantstage.py")
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, tree)
	cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(tree, "xdg-state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home,
		"PYTHONPATH="+filepath.Join(root, "packages", "codex-session-relay", "src")+":"+filepath.Join(root, "packages", "codex-session-relay"))
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	mustDo(t, err)
	outPipe, err := cmd.StdoutPipe()
	mustDo(t, err)
	mustDo(t, cmd.Start())
	g := &grantStage{t: t, cmd: cmd, in: in, out: bufio.NewScanner(outPipe)}
	g.out.Buffer(make([]byte, 1<<20), 1<<24)
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })
	if !g.out.Scan() {
		t.Fatalf("grant stage printed nothing: %v", g.out.Err())
	}
	mustDo(t, json.Unmarshal(g.out.Bytes(), &g.staged))
	return g
}

func (g *grantStage) do(command string) {
	g.t.Helper()
	if _, err := io.WriteString(g.in, command+"\n"); err != nil {
		g.t.Fatal(err)
	}
	if !g.out.Scan() {
		g.t.Fatalf("grant stage stopped after %q: %v", command, g.out.Err())
	}
}

// grantHost is a fake host that holds the staged transport receipt, so reconciliation reads
// what Python's adapter answered for the notice.
func grantHost(h *hl, g *grantStage) {
	for _, thread := range g.staged.Threads {
		if h.host.threads[thread] == nil {
			h.host.addThread(thread)
		}
	}
	request := g.staged.Record["requestId"].(string)
	receipt := Obj{}
	for _, k := range []string{"requestId", "operation", "status", "threadId", "retrySafe", "error"} {
		if v, ok := g.staged.Receipt[k]; ok {
			receipt = append(receipt, F{Key: k, Value: v})
		}
	}
	h.host.ledger[request] = receipt
	for _, s := range g.staged.Sends {
		h.host.sends = append(h.host.sends, fakeSend{s[0].(string), s[1].(string), s[2].(string), s[3].(string)})
	}
}

func runGrant(t *testing.T, name string, answer func(h *hl, g *grantStage)) {
	root := pythonCaptures(t, usl)
	raw, err := os.ReadFile(filepath.Join(root, name, "capture.json"))
	mustDo(t, err)
	var python pyCapture
	mustDo(t, json.Unmarshal(raw, &python))
	if len(python.Problems) > 0 {
		t.Fatalf("the Python test itself failed: %s", python.Problems)
	}
	tree := t.TempDir()
	g := startGrantStage(t, tree)
	s, err := store.Open(context.Background(), g.staged.Store, "")
	mustDo(t, err)
	t.Cleanup(func() { _ = s.Close() })
	clock := &FakeClock{T: g.staged.Now}
	f := &fixture{t: t, ctx: context.Background(), tree: tree, clock: clock, store: s, host: newFakeHost(clock)}
	f.delivery = NewService(s, clock)
	h := &hl{fixture: f, name: name, ack: NewAck(f.delivery), rc: NewReconciler(f.delivery), adapter: f.host, policy: defaultTick()}
	grantHost(h, g)
	event := g.staged.Event
	request := g.staged.Record["requestId"].(string)
	// uncertain_grant
	h.eq(g.staged.Waiting)
	h.eq(g.staged.Record["transportReceiptStatus"])
	h.clock.Advance(120)
	g.do("advance 120")
	h.reconcile(request, h.host)
	row := h.row(event)
	h.eq(row.S("state"))
	h.eq(row.S("hold_reason") == UnknownSendLost || row.S("hold_reason") == UnknownSendUndecided)
	answer(h, g)
	outcome := h.reconcile(request, h.host)
	h.eq([]any{field(outcome, "nextExpectedAction"), field(outcome, "reason")})
	_, has := get(outcome, "recovery")
	h.eq(has)
	h.eq(h.statusPair(event))
	if name == "AnUncertainGrantIsReadAsStatusReadsIt.test_an_uncertain_grant_answered_on_its_turn_reads_acknowledged" {
		h.eq(h.row(event).S("state"))
	}
	h.eq(h.sendsTo(parent))
	_, _ = io.WriteString(g.in, "quit\n")
	requireSameCaptures(t, h.got, python.Captures)
}

func Test21_USL23_an_uncertain_grant_is_read_as_status_reads_it(t *testing.T) {
	const cls = "AnUncertainGrantIsReadAsStatusReadsIt."
	t.Run("answered on its turn", func(t *testing.T) {
		runGrant(t, cls+"test_an_uncertain_grant_answered_on_its_turn_reads_acknowledged", func(h *hl, g *grantStage) { g.do("answer") })
	})
	t.Run("regranted meanwhile", func(t *testing.T) {
		runGrant(t, cls+"test_an_uncertain_grant_regranted_meanwhile_reads_superseded", func(h *hl, g *grantStage) { g.do("regrant") })
	})
}
