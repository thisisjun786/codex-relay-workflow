package delivery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// VCU-13: closing the CLI's services releases what they opened and builds nothing just to
// close it. The Go CLI owns one store per invocation and no host adapter until one is needed.
func TestVCU13_the_cli_closes_what_it_opened_and_builds_nothing_to_close(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xs"))
	state := filepath.Join(home, "state")
	var out, errw bytes.Buffer

	code, handled := ExecuteCLI(context.Background(), []string{"--state", state, "ack-proof", "--event", "0123456789abcdef0123456789abcdef", "--turn", "t"}, &out, &errw)
	if !handled || code != 0 {
		t.Fatalf("ack-proof %d %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
		t.Fatal("a command that needs no store built one")
	}
	out.Reset()
	code, _ = ExecuteCLI(context.Background(), []string{"--state", state, "deliver"}, &out, &errw)
	if code != 4 {
		t.Fatalf("deliver without a socket is a usage error, got %d", code)
	}
	out.Reset()
	code, _ = ExecuteCLI(context.Background(), []string{"--state", state, "criteria-register", "--relationship", "r", "--criterion", "c1=one"}, &out, &errw)
	if code != 0 {
		t.Fatalf("criteria-register %d %s", code, out.String())
	}
	// The store the command opened is closed: a writer with no busy wait takes the lock at once.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	mustDo(t, err)
	defer s.Close()
	mustDo(t, s.AppendJournal(ctx, store.JournalEntry{At: "t", Kind: "probe", Subject: "s", Detail: "{}"}))
}
