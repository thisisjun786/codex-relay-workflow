package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// CRW-1194: journal-tail and receive are relay commands added to the command surface. They are
// not added to the lists doctor reports, which are the contract's: the lists are the ones the cut
// ref has, the digest below being taken over them.

// listsDigest is sha256 over the two lists as doctor reports them, the offline one first. The offline list is the
// contract's less the three child report commands (emit, intent-claim, intent-disposition), which doctor lists in
// daemonRequiredCommands instead (CRW-1298); the host-required list is the contract's plus receipt-pull (CRW-1310), which needs the host to judge the live turn.
const listsDigest = "0774afcb664c5d9617ef66711d5287bf45a092cf16c6075199031f7ebb50f3cd"

func TestDoctorCommandListsUnchanged(t *testing.T) {
	state := ownerOnlyState(t, tempHome(t), "doctor")
	got := goCLI(t, "--state", state, "doctor")
	if got.code != 0 {
		t.Fatalf("doctor: exit %d %s", got.code, got.stdout)
	}
	reach, _ := object(t, got.stdout)["actorReachability"].(map[string]any)
	var lists [2][]string
	for i, key := range []string{"offlineCommands", "hostRequiredCommands"} {
		items, _ := reach[key].([]any)
		for _, item := range items {
			lists[i] = append(lists[i], item.(string))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lists[0], "\n") + "\n--\n" + strings.Join(lists[1], "\n")))
	if len(lists[0]) != 144 || len(lists[1]) != 12 || hex.EncodeToString(sum[:]) != listsDigest {
		t.Errorf("doctor's command lists moved: %d offline, %d host-required, digest %s", len(lists[0]), len(lists[1]), hex.EncodeToString(sum[:]))
	}
	for _, name := range []string{"journal-tail", "receive"} {
		if _, ok := dispatch.Lookup(name); !ok {
			t.Errorf("%s is not a registered relay command", name)
		}
		if slices.Contains(lists[0], name) || slices.Contains(lists[1], name) {
			t.Errorf("%s is in a doctor list", name)
		}
	}
}

// receive on the command line: its parser declares the three options and no proof, a verdict file it
// cannot read is a usage error that writes nothing, and a refusal of a step is that step's refusal.
func TestReceiveCommandLine(t *testing.T) {
	state := ownerOnlyState(t, tempHome(t), "receive")
	dir := t.TempDir()
	file := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(file, []byte(`{"verdict":"verified"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	event := strings.Repeat("ab", 16)
	line := func(extra ...string) answer {
		return goCLI(t, append([]string{"--state", state, "receive"}, extra...)...)
	}
	for _, extra := range [][]string{
		{"--event", event, "--ack-turn", "t1"},
		{"--event", event, "--verdict-file", file},
		{"--ack-turn", "t1", "--verdict-file", file},
		{"--event", event, "--ack-turn", "t1", "--verdict-file", file, "--ack-proof", "x"},
	} {
		if got := line(extra...); got.code != 2 || got.stdout != "" {
			t.Errorf("receive %v: exit %d %q, wanted the parser's refusal", extra, got.code, got.stdout)
		}
	}
	for name, content := range map[string]string{"not JSON": `{`, "a verdict turn": `{"verdict":"verified","verdict-turn":"t9"}`, "no verdict": `{}`} {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := line("--event", event, "--ack-turn", "t1", "--verdict-file", bad); got.code != 4 {
			t.Errorf("%s: exit %d %s, wanted the usage error 4", name, got.code, got.stdout)
		}
	}
	if got := line("--event", event, "--ack-turn", "t1", "--verdict-file", filepath.Join(dir, "absent.json")); got.code != 4 {
		t.Errorf("an absent verdict file: exit %d %s", got.code, got.stdout)
	}
	// nothing was delivered for the event: the claim is recorded, and the ack step refuses as ack does
	got := line("--event", event, "--ack-turn", "t1", "--verdict-file", file)
	if got.code != 2 || object(t, got.stdout)["reason"] != "not_claimable" {
		t.Fatalf("an event nothing was delivered for: exit %d %s", got.code, got.stdout)
	}
	same := goCLI(t, "--state", state, "ack", "--event", event, "--ack-turn", "t1", "--ack-proof", strings.Repeat("0", 64))
	if same.code != 2 || object(t, same.stdout)["reason"] != "not_claimable" || object(t, same.stdout)["detail"] != object(t, got.stdout)["detail"] {
		t.Errorf("ack refuses %s, receive %s", same.stdout, got.stdout)
	}
	// a malformed event id is refused as ack-proof refuses it
	if bad := line("--event", "nope", "--ack-turn", "t1", "--verdict-file", file); bad.code != 3 || !strings.Contains(bad.stdout, "event id must be 32 lowercase hex characters") {
		t.Errorf("a malformed event id: exit %d %s", bad.code, bad.stdout)
	}
}

// receive judges its whole line before the store is admitted: a line that is refused as usage (or as a
// malformed id) leaves the state directory as it was, creates no store and binds none to the socket
// the line named. A store the same socket-less state later gets is the caller's own.
func TestReceiveRefusedLineTouchesNoStore(t *testing.T) {
	home := tempHome(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"verdict":"verified"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	badHead := filepath.Join(dir, "head.json")
	if err := os.WriteFile(badHead, []byte(`{"verdict":"verified","verified-head":"XYZ"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	event := strings.Repeat("ab", 16)
	wrong := filepath.Join(home, "wrong.sock")
	for i, c := range []struct {
		file, event, turn string
		code              int
	}{
		{malformed, event, "t1", 4},
		{filepath.Join(dir, "absent.json"), event, "t1", 4},
		{badHead, event, "t1", 4},
		{good, "nope", "t1", 3},
		{good, event, "  ", 3},
	} {
		state := ownerOnlyState(t, home, "refused-"+strconv.Itoa(i))
		got := goCLI(t, "--state", state, "--socket", wrong, "receive", "--event", c.event, "--ack-turn", c.turn, "--verdict-file", c.file)
		if got.code != c.code {
			t.Errorf("case %d: exit %d %s, wanted %d", i, got.code, got.stdout, c.code)
		}
		if entries, _ := os.ReadDir(state); len(entries) != 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("case %d: the refused line left %v in the state directory", i, names)
		}
	}
}
