package managed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test27_MST_5_SelectorSpellingAndInputFingerprint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	request, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	ledger := map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}
	root := filepath.Join(dir, "markers")
	socket := filepath.Join(dir, "socket")
	first, err := RequestIdentity(ctx, request, s, socket, root, dir, ledger)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := RequestIdentity(ctx, request, s, socket, root, dir, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if first != replay || first.Fingerprint == "" {
		t.Fatalf("unstable identity: %+v %+v", first, replay)
	}
	alias, err := RequestIdentity(ctx, request, s, socket, root+"/alias/..", dir, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if alias.Fingerprint == first.Fingerprint || alias.MarkerRoot != first.MarkerRoot {
		t.Fatalf("original spelling lost: %+v %+v", first, alias)
	}
	spelling := filepath.Join(dir, "spelling")
	if err := os.Mkdir(spelling, 0700); err != nil {
		t.Fatal(err)
	}
	for name, selectors := range map[string][3]string{"socket": {spelling + "/../socket", root, dir}, "state": {socket, root, spelling + "/.."}, "markerRoot": {socket, spelling + "/../markers", dir}} {
		variant, e := RequestIdentity(ctx, request, s, selectors[0], selectors[1], selectors[2], ledger)
		if e != nil {
			t.Fatal(e)
		}
		if variant.Fingerprint == first.Fingerprint {
			t.Fatalf("%s spelling lost", name)
		}
	}
	var changed map[string]any
	if err := json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	changed["prompt"] = "another business assignment"
	other, err := RequestIdentity(ctx, changed, s, socket, root, dir, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if other.Fingerprint == first.Fingerprint || other.DispatchRequestID != first.DispatchRequestID {
		t.Fatalf("input identity: %+v %+v", first, other)
	}
}
