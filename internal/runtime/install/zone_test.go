package install_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The additive DAG zone on the supported install path (D-01, OPS-4.5): a store that predates the zone is installed
// onto only through --backup-state-to, which copies the whole state directory before the swap; a store that holds the
// zone does not refuse a candidate that does not declare it; everything else refuses as it always has.

// zoneStore is a relay state directory the way every production one is: a store built from the frozen v1 schema, with
// no DAG zone, beside the other things a state directory holds (a ledger file, a nested directory, a link to a file
// elsewhere, a FIFO).
func zoneStore(t *testing.T, h *host) (outside string) {
	t.Helper()
	testsupport.Create(t, filepath.Join(h.relayState, "relay.sqlite3"), "", "go")
	write(t, filepath.Join(h.relayState, "ledger.log"), "line one\nline two\n")
	write(t, filepath.Join(h.relayState, "scopes", "one", "operations.log"), "nested\n")
	outside = filepath.Join(t.TempDir(), "linked-target")
	write(t, outside, "the bytes behind a link\n")
	if err := os.Symlink(outside, filepath.Join(h.relayState, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(h.relayState, "control.fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a link that goes through another link: state/indirect.log -> chain/current/ledger, current -> chain/one
	chain := t.TempDir()
	write(t, filepath.Join(chain, "one", "ledger"), "OLD\n")
	write(t, filepath.Join(chain, "two", "ledger"), "NEW\n")
	if err := os.Symlink(filepath.Join(chain, "one"), filepath.Join(chain, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(chain, "current", "ledger"), filepath.Join(h.relayState, "indirect.log")); err != nil {
		t.Fatal(err)
	}
	return outside
}

// digestTree is every regular file under dir (links followed to the file they name) by relative path and digest,
// and every directory, as an independent reading of what a directory holds.
func digestTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			out[rel] = "dir"
		case info.Mode().IsRegular():
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			out[rel] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, want, got map[string]string, what string) {
	t.Helper()
	var wrong []string
	for k, v := range want {
		if got[k] != v {
			wrong = append(wrong, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			wrong = append(wrong, k+" (not in the source)")
		}
	}
	sort.Strings(wrong)
	if len(wrong) > 0 {
		t.Fatalf("%s differs: %v", what, wrong)
	}
}

// withoutSidecars drops SQLite's -wal and -shm files: the relay's own read of a WAL-mode store (the in-flight cell asks
// the selected relay's doctor) leaves them beside the database, before and without any change of this work.
func withoutSidecars(tree map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range tree {
		if !strings.HasSuffix(k, "-wal") && !strings.HasSuffix(k, "-shm") {
			out[k] = v
		}
	}
	return out
}

// atCopy records the state directory at the moment the backup has copied it (before its verification): the source the
// copy must equal, read independently of the manifest.
func atCopy(t *testing.T, h *host) (seen *map[string]string, restore func()) {
	t.Helper()
	var snapshot map[string]string
	restore = install.ReplaceStateBackupStep(func(string) error { snapshot = digestTree(t, h.relayState); return nil })
	return &snapshot, restore
}

func nothingAt(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists (%v)", path, err)
	}
}

// zoneInstalled puts one runtime on the host (no store yet); the test then makes the store, so the update is the
// install that brings the zone.
func zoneInstalled(t *testing.T) (h *host, first, second, old, next string) {
	t.Helper()
	h = newHost(t)
	first, second = archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, next = runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	return h, first, second, old, next
}

func backupOf(h *host, name string) string { return filepath.Join(h.home, "backups", name) }

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTheZoneArrivalIsRefusedWithoutTheRouteAndTheRefusalNamesIt(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	before := digestTree(t, h.relayState)
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	cell := golden.Canon(at(result, "swapGate", "cells", "storeSchema"))
	if code != install.Refused || at(result, "failedStep") != "read whether it is safe to swap" || at(result, "swapGate", "verdict") != "BLOCKED" ||
		at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone || !strings.Contains(cell, "--backup-state-to") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if h.pointerTarget(t) != old || at(result, "retriable") != true {
		t.Fatalf("pointer %s", h.pointerTarget(t))
	}
	sameTree(t, withoutSidecars(before), withoutSidecars(digestTree(t, h.relayState)), "the state directory")
}

func TestTheZoneArrivalWithTheRouteIsBackedUpByteForByteAndThenSwaps(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	outside := zoneStore(t, h)
	before := digestTree(t, h.relayState)
	seen, restore := atCopy(t, h)
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "arrival")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || at(result, "swapGate", "verdict") != "ALLOWED" || at(result, "swapGate", "stateBackup", "made") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if h.pointerTarget(t) != next {
		t.Fatalf("the swap did not happen: %s", h.pointerTarget(t))
	}

	// the copy, read independently of the manifest: every directory and every file of the source, the link as the
	// bytes behind it, the FIFO absent, and the source itself as it was
	backup := backupOf(h, "arrival")
	sameTree(t, *seen, digestTree(t, backup), "the backup")
	sameTree(t, withoutSidecars(before), withoutSidecars(digestTree(t, h.relayState)), "the state directory after the swap")
	if got := mustRead(t, filepath.Join(backup, "linked.txt")); !bytes.Equal(got, mustRead(t, outside)) {
		t.Fatalf("the link's bytes: %q", got)
	}
	if info, err := os.Lstat(filepath.Join(backup, "linked.txt")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the link is copied as the file it names: %v %v", info, err)
	}
	nothingAt(t, filepath.Join(backup, "control.fifo"))
	if !bytes.Equal(mustRead(t, filepath.Join(backup, "relay.sqlite3")), mustRead(t, filepath.Join(h.relayState, "relay.sqlite3"))) {
		t.Fatal("the store is not byte-identical in the backup")
	}

	// the manifest sits beside the backup, never inside it, and its digests are the files'
	manifestPath := backup + install.ManifestSuffix
	var manifest struct {
		Destination     string   `json:"destination"`
		AggregateDigest string   `json:"aggregateDigest"`
		Files           int      `json:"files"`
		Skipped         []string `json:"skipped"`
		Entries         []struct {
			Path   string `json:"path"`
			Kind   string `json:"kind"`
			SHA256 string `json:"sha256"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(mustRead(t, manifestPath), &manifest); err != nil {
		t.Fatal(err)
	}
	nothingAt(t, filepath.Join(backup, filepath.Base(manifestPath)))
	if manifest.Destination != backup || len(manifest.Skipped) != 1 || !strings.Contains(manifest.Skipped[0], "control.fifo") {
		t.Fatalf("manifest: %+v", manifest)
	}
	files := 0
	for _, e := range manifest.Entries {
		if e.Kind == "dir" {
			continue
		}
		files++
		if (*seen)[filepath.FromSlash(e.Path)] != e.SHA256 {
			t.Errorf("manifest digest of %s", e.Path)
		}
	}
	if files == 0 || files != manifest.Files || at(result, "swapGate", "stateBackup", "aggregateDigest") != manifest.AggregateDigest || at(result, "swapGate", "stateBackup", "manifest") != manifestPath {
		t.Fatalf("the result records the copy: %s", golden.Canon(at(result, "swapGate", "stateBackup")))
	}
}

// The route is for the zone arriving alone. Any other difference refuses with the acknowledgement as without it, and
// no backup is made for it.
func TestTheRouteDoesNotCarryAnythingButTheZoneArrivingAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, db *sql.DB)
		answer  string
	}{
		"an object of the store the candidate does not declare": {func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec("CREATE TABLE only_in_this_store (x)"); err != nil {
				t.Fatal(err)
			}
		}, "NARROWS"},
		"a frozen table defined differently": {func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec("ALTER TABLE deliveries ADD COLUMN extra TEXT"); err != nil {
				t.Fatal(err)
			}
		}, "DIFFERS"},
		"a zone table defined differently": {func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec("CREATE TABLE dag_plans (plan_id TEXT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
		}, "DIFFERS"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, second, old, _ := zoneInstalled(t)
			zoneStore(t, h)
			db, err := sql.Open("sqlite", filepath.Join(h.relayState, "relay.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, db)
			_ = db.Close()
			o := h.options()
			o.StateBackup = backupOf(h, "refused")
			result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
			if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "cells", "storeSchema", "answer") != tc.answer {
				t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
			}
			if h.pointerTarget(t) != old {
				t.Fatal("the pointer moved")
			}
			nothingAt(t, backupOf(h, "refused"))
			nothingAt(t, backupOf(h, "refused")+install.ManifestSuffix)
			if at(result, "swapGate", "stateBackup", "made") != false {
				t.Fatalf("a backup was reported: %s", golden.Canon(at(result, "swapGate", "stateBackup")))
			}
		})
	}
}

// An open attempt refuses the swap whatever else holds, so it refuses the zone arrival that was acknowledged, and no
// backup is made for a swap that does not happen.
func TestAnOpenAttemptStillRefusesTheAcknowledgedArrivalAndNothingIsCopied(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	db, err := sql.Open("sqlite", filepath.Join(h.relayState, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, created_at, updated_at) VALUES ('e1', 'rel', 'completion_event', 't', 'th', 'sending', 'x', 'x')",
		"INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, observed_at) VALUES ('r1', 'e1', 1, 'completion_event', 'in_flight', 'x')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	o := h.options()
	o.StateBackup = backupOf(h, "open")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone ||
		at(result, "swapGate", "cells", "inFlight", "answer") == int64(0) {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if h.pointerTarget(t) != old {
		t.Fatal("the pointer moved")
	}
	nothingAt(t, backupOf(h, "open"))
	nothingAt(t, backupOf(h, "open")+install.ManifestSuffix)
}

// Where the backup may not go: over anything, into the state directory, or into the install's own tree, which a failed
// run removes. Each is refused before a byte is copied.
func TestTheBackupDestinationIsRefusedWhereItCouldNotSurvive(t *testing.T) {
	h, _, second, old, next := zoneInstalled(t)
	zoneStore(t, h)
	before := digestTree(t, h.relayState)
	existing := backupOf(h, "taken")
	write(t, filepath.Join(existing, "somebody"), "x")
	withManifest := backupOf(h, "manifest-taken")
	write(t, withManifest+install.ManifestSuffix, "{}")
	for name, dest := range map[string]string{
		"a destination that exists":                      existing,
		"a manifest that exists":                         withManifest,
		"the state directory itself":                     h.relayState,
		"inside the state directory":                     filepath.Join(h.relayState, "backup"),
		"inside the runtime destination tree":            filepath.Join(h.dest, "backup"),
		"inside the candidate runtime":                   filepath.Join(next, "backup"),
		"inside a runtime directory that is installed":   filepath.Join(old, "backup"),
		"through the pointer into the installed runtime": filepath.Join(pointer.Path(h.dest), "backup"),
	} {
		o := h.options()
		o.StateBackup = dest
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "stateBackup", "made") != false || at(result, "swapGate", "stateBackup", "partial") != false {
			t.Errorf("%s: exit %d\n%s", name, code, golden.Canon(at(result, "swapGate")))
		}
		if h.pointerTarget(t) != old {
			t.Fatalf("%s: the pointer moved", name)
		}
	}
	sameTree(t, withoutSidecars(before), withoutSidecars(digestTree(t, h.relayState)), "the state directory")
	if _, err := os.Stat(filepath.Join(existing, "somebody")); err != nil {
		t.Fatal("an existing destination was touched")
	}
}

func appendTo(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// A backup that fails part-way, or whose source moved while it was copied, refuses the swap, and what was copied stays:
// nothing is deleted, there is no manifest to vouch for it, and the pointer is where it was.
func TestABackupThatFailsOrIsNotOfOneMomentRefusesTheSwapAndKeepsWhatItCopied(t *testing.T) {
	for name, tc := range map[string]struct {
		step func(h *host) error
		want string
	}{
		"the copy fails":             {func(h *host) error { return errors.New("injected: the disk filled up") }, "injected"},
		"a file of the ledger grows": {func(h *host) error { return appendTo(filepath.Join(h.relayState, "ledger.log"), "a new line\n") }, "changed"},
		"a byte of the ledger changes in place": {func(h *host) error {
			return os.WriteFile(filepath.Join(h.relayState, "ledger.log"), []byte("line ONE\nline two\n"), 0o600)
		}, "changed"},
		"a copied file is damaged": {func(h *host) error {
			return os.WriteFile(filepath.Join(backupOf(h, "partial"), "ledger.log"), []byte("damaged"), 0o600)
		}, "copy of ledger.log"},
		"a link in the chain behind a linked file is repointed": {func(h *host) error {
			target, err := os.Readlink(filepath.Join(h.relayState, "indirect.log"))
			if err != nil {
				return err
			}
			current := filepath.Dir(target)
			if err := os.Remove(current); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(filepath.Dir(current), "two"), current)
		}, "changed"},
		"a file of the store grows": {func(h *host) error { return appendTo(filepath.Join(h.relayState, "relay.sqlite3"), "x") }, "changed"},
		"a file is added":           {func(h *host) error { return os.WriteFile(filepath.Join(h.relayState, "new.log"), []byte("n"), 0o600) }, "changed"},
		"a file goes":               {func(h *host) error { return os.Remove(filepath.Join(h.relayState, "ledger.log")) }, "changed"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, second, old, _ := zoneInstalled(t)
			zoneStore(t, h)
			restore := install.ReplaceStateBackupStep(func(string) error { return tc.step(h) })
			defer restore()
			o := h.options()
			o.StateBackup = backupOf(h, "partial")
			result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
			if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "stateBackup", "made") != false ||
				at(result, "swapGate", "stateBackup", "partial") != true || at(result, "swapGate", "stateBackup", "kept") != true ||
				!strings.Contains(text(at(result, "swapGate", "stateBackup", "error")), tc.want) {
				t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
			}
			if h.pointerTarget(t) != old {
				t.Fatal("the pointer moved")
			}
			if _, err := os.Stat(backupOf(h, "partial")); err != nil {
				t.Fatalf("what was copied was removed: %v", err)
			}
			nothingAt(t, backupOf(h, "partial")+install.ManifestSuffix)
		})
	}
}

// A failure after the backup leaves it where it is: the pointer move that fails is put back, and the copy is a faithful
// copy of a store nothing wrote.
func TestTheBackupSurvivesAPromotionThatFailsAfterIt(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	seen, observe := atCopy(t, h)
	defer observe()
	restore := install.ReplacePointerPlacement(func(path, target string) error { return errors.New("injected: the pointer move failed") })
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "kept")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if h.pointerTarget(t) != old {
		t.Fatal("the pointer moved")
	}
	if at(result, "swapGate", "stateBackup", "made") != true || at(result, "swapGate", "stateBackup", "kept") != true {
		t.Fatalf("the failure reports the backup it kept: %s", golden.Canon(at(result, "swapGate")))
	}
	sameTree(t, *seen, digestTree(t, backupOf(h, "kept")), "the backup")
}

// The acknowledgement for a swap that needs none is not a refusal and takes no backup.
func TestTheAcknowledgementWhereTheZoneDoesNotArriveTakesNoBackup(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	o := h.options()
	o.StateBackup = backupOf(h, "unneeded")
	if result, code := install.Install(context.Background(), o, "install", install.Source{From: first}); code != install.OK || at(result, "swapGate", "stateBackup", "made") != false {
		t.Fatalf("a first install, no store: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	// a store this build already opened: the zone is there, nothing arrives
	openedByThisBuild(t, h)
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "swapGate", "verdict") != "ALLOWED" || at(result, "swapGate", "stateBackup", "made") != false || at(result, "swapGate", "stateBackup", "requested") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	nothingAt(t, backupOf(h, "unneeded"))
}

// withoutZone is a candidate that does not declare the zone: an older runtime.
func withoutZone(context.Context, string) record.Object {
	declared := swapgate.DeclaredSchema(context.Background())
	var objects record.Object
	for _, field := range golden.Obj(record.Get(declared, "objects")) {
		if !strings.Contains(field.Key, " dag_") {
			objects = append(objects, field)
		}
	}
	return record.Object{{Key: "readable", Value: true}, {Key: "objects", Value: objects}, {Key: "schemaVersion", Value: store.SchemaVersion}, {Key: "detail", Value: nil}}
}

func openedByThisBuild(t *testing.T, h *host) {
	t.Helper()
	path := filepath.Join(h.relayState, "relay.sqlite3")
	if _, err := os.Lstat(path); err != nil {
		testsupport.Create(t, path, "", "go")
	}
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}

// Returning to a runtime that does not declare the zone is not refused for the zone, and needs no acknowledgement and no
// backup: on a promotion, on a promotion an interrupted run left to finish, and on a rollback that moves the pointer.
func TestAStoreThatOnlyHoldsTheZoneDoesNotRefuseARuntimeThatDoesNotDeclareIt(t *testing.T) {
	t.Run("an update", func(t *testing.T) {
		h, _, second, _, next := zoneInstalled(t)
		openedByThisBuild(t, h)
		o := h.options()
		o.CandidateSchema = withoutZone
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.OK || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.NarrowsZone || at(result, "swapGate", "verdict") != "ALLOWED" || h.pointerTarget(t) != next {
			t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
		}
	})
	t.Run("an interrupted promotion finished by a rerun", func(t *testing.T) {
		h, _, second, old, next := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
			t.Fatal(err)
		}
		write(t, staging.ClaimPath(next), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
		openedByThisBuild(t, h)
		o := h.options()
		o.CandidateSchema = withoutZone
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.OK || at(result, "resumed") != true || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.NarrowsZone || h.pointerTarget(t) != next {
			t.Fatalf("exit %d\n%s", code, golden.Canon(result))
		}
	})
	t.Run("a rollback", func(t *testing.T) {
		h, _, second, old, _ := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		openedByThisBuild(t, h)
		o := h.options()
		o.CandidateSchema = withoutZone
		result, code := install.Rollback(context.Background(), o, "")
		if code != install.OK || at(result, "swapGate", "cells", "storeSchema", "answer") != swapgate.NarrowsZone || h.pointerTarget(t) != old {
			t.Fatalf("exit %d\n%s", code, golden.Canon(result))
		}
	})
}

// The route is on every path that moves the pointer: an interrupted promotion finished by a rerun, and a rollback to a
// runtime that declares the zone while the store still lacks it.
func TestTheRouteIsOnTheResumeAndOnTheRollbackToo(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		h, _, second, old, next := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
			t.Fatal(err)
		}
		write(t, staging.ClaimPath(next), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
		zoneStore(t, h)
		refused, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
		if code != install.Refused || at(refused, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone || h.pointerTarget(t) != old ||
			!strings.Contains(golden.Canon(at(refused, "swapGate")), "--backup-state-to") {
			t.Fatalf("without the route: exit %d\n%s", code, golden.Canon(refused))
		}
		// a backup that fails: refused, pointer and selection unchanged, the partial copy kept, no manifest
		failing := install.ReplaceStateBackupStep(func(string) error { return errors.New("injected: the disk filled up") })
		o := h.options()
		o.StateBackup = backupOf(h, "resume-partial")
		failed, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		failing()
		if code != install.Refused || at(failed, "swapGate", "stateBackup", "partial") != true || h.pointerTarget(t) != old {
			t.Fatalf("a failing backup: exit %d\n%s", code, golden.Canon(failed))
		}
		nothingAt(t, backupOf(h, "resume-partial")+install.ManifestSuffix)
		o.StateBackup = backupOf(h, "resume")
		seen, observe := atCopy(t, h)
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		observe()
		if code != install.OK || at(result, "resumed") != true || at(result, "swapGate", "stateBackup", "made") != true || h.pointerTarget(t) != next {
			t.Fatalf("with the route: exit %d\n%s", code, golden.Canon(result))
		}
		sameTree(t, *seen, digestTree(t, backupOf(h, "resume")), "the backup")
	})
	t.Run("rollback", func(t *testing.T) {
		h, _, second, old, next := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		zoneStore(t, h)
		refused, code := install.Rollback(context.Background(), h.options(), "")
		if code != install.Refused || at(refused, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone || h.pointerTarget(t) != next ||
			!strings.Contains(golden.Canon(at(refused, "swapGate")), "--backup-state-to") {
			t.Fatalf("without the route: exit %d\n%s", code, golden.Canon(refused))
		}
		failing := install.ReplaceStateBackupStep(func(string) error { return errors.New("injected: the disk filled up") })
		o := h.options()
		o.StateBackup = backupOf(h, "rollback-partial")
		failed, code := install.Rollback(context.Background(), o, "")
		failing()
		if code != install.Refused || at(failed, "swapGate", "stateBackup", "partial") != true || h.pointerTarget(t) != next {
			t.Fatalf("a failing backup: exit %d\n%s", code, golden.Canon(failed))
		}
		nothingAt(t, backupOf(h, "rollback-partial")+install.ManifestSuffix)
		o.StateBackup = backupOf(h, "rollback")
		seen, observe := atCopy(t, h)
		result, code := install.Rollback(context.Background(), o, "")
		observe()
		if code != install.OK || at(result, "swapGate", "stateBackup", "made") != true || h.pointerTarget(t) != old {
			t.Fatalf("with the route: exit %d\n%s", code, golden.Canon(result))
		}
		sameTree(t, *seen, digestTree(t, backupOf(h, "rollback")), "the backup")
	})
}

// The command line: --backup-state-to is on install, update and rollback, and nowhere else; an empty one names no
// directory.
func TestTheBackupFlagIsOnTheCommandsThatSwapAndNowhereElse(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	common := []string{"--codex-home", h.codex, "--record", h.record, "--socket", h.fake.SocketPath, "--state", h.relayState}
	run := func(args ...string) (string, string, int) {
		var stdout, stderr strings.Builder
		code := install.Main(context.Background(), append(args, common...), h.env, &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	if _, stderr, code := run("update", "--from", second, "--backup-state-to", ""); code != install.Usage || !strings.Contains(stderr, "names no directory") {
		t.Fatalf("empty: exit %d %s", code, stderr)
	}
	for _, command := range []string{"status", "remove"} {
		args := []string{command, "--backup-state-to", backupOf(h, "nowhere")}
		if command == "remove" {
			args = append(args, filepath.Join(h.dest, "bin-nothing"))
		}
		if _, _, code := run(args...); code != install.Usage {
			t.Errorf("%s accepts --backup-state-to (exit %d)", command, code)
		}
	}
	if stdout, _, code := run("update", "--from", second); code != install.Refused || !strings.Contains(stdout, "--backup-state-to") {
		t.Fatalf("without it: exit %d\n%s", code, stdout)
	}
	seen, observe := atCopy(t, h)
	stdout, stderr, code := run("update", "--from", second, "--backup-state-to", backupOf(h, "cli"))
	observe()
	if code != install.OK || !strings.Contains(stdout, "\"made\": true") || h.pointerTarget(t) != next {
		t.Fatalf("with it: exit %d\n%s\n%s", code, stdout, stderr)
	}
	sameTree(t, *seen, digestTree(t, backupOf(h, "cli")), "the backup")
}

// A store whose database is a link to a file elsewhere keeps its write-ahead log beside that file: the backup carries
// the log too, so a commit that only the log held is in the backup, and the backup opened on its own has it.
func TestTheBackupOfALinkedStoreCarriesItsLog(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	elsewhere := t.TempDir()
	real := filepath.Join(elsewhere, "real.sqlite3")
	testsupport.Create(t, real, "", "go")
	// a commit that stays in the log: autocheckpoint is off and the connection stays open
	db, err := sql.Open("sqlite", real)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "INSERT INTO schema_meta (key, value) VALUES ('probe', 'only in the log')"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real, filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(real + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the commit is not in the log: %v", err)
	}
	o := h.options()
	o.StateBackup = backupOf(h, "linked")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "swapGate", "stateBackup", "made") != true || h.pointerTarget(t) != next {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	backup := backupOf(h, "linked")
	for name, source := range map[string]string{"relay.sqlite3": real, "relay.sqlite3-wal": real + "-wal"} {
		if !bytes.Equal(mustRead(t, filepath.Join(backup, name)), mustRead(t, source)) {
			t.Errorf("%s of the backup is not what the store holds", name)
		}
	}
	if _, err := os.Stat(filepath.Join(backup, "relay.sqlite3-shm")); err != nil {
		t.Errorf("the index of the log is not in the backup: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(backup, "relay.sqlite3")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the link is copied as the file it names: %v %v", info, err)
	}
	// the backup, opened on its own, holds the row that only the log held
	copied := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(copied, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay.sqlite3", "relay.sqlite3-wal", "relay.sqlite3-shm"} {
		if err := os.WriteFile(filepath.Join(copied, name), mustRead(t, filepath.Join(backup, name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := sql.Open("sqlite", filepath.Join(copied, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var got string
	if err := restored.QueryRow("SELECT value FROM schema_meta WHERE key = 'probe'").Scan(&got); err != nil || got != "only in the log" {
		t.Fatalf("the backup does not hold the commit that only the log held: %q, %v", got, err)
	}
}

// A refusal after the backup was taken still reports it, on every path that moves the pointer: the copy is there, it is
// kept, and the result says where, so "nothing was written" is never told of a run that wrote a backup.
func TestARefusalAfterTheBackupStillReportsItOnResumeAndOnRollback(t *testing.T) {
	failPlacement := func() func() {
		return install.ReplacePointerPlacement(func(path, target string) error { return errors.New("injected: the pointer move failed") })
	}
	check := func(t *testing.T, h *host, result record.Object, code int, backup string, pointerWas string) {
		t.Helper()
		if code != install.Refused || at(result, "swapGate", "stateBackup", "made") != true || at(result, "swapGate", "stateBackup", "kept") != true ||
			at(result, "swapGate", "stateBackup", "destination") != backup || !strings.Contains(text(at(result, "note")), "stays at "+backup) {
			t.Fatalf("exit %d\n%s\nnote: %v", code, golden.Canon(at(result, "swapGate")), at(result, "note"))
		}
		if _, err := os.Stat(backup + install.ManifestSuffix); err != nil {
			t.Fatalf("the manifest of the kept backup: %v", err)
		}
		if h.pointerTarget(t) != pointerWas {
			t.Fatal("the pointer moved")
		}
	}
	t.Run("resume", func(t *testing.T) {
		h, _, second, old, next := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
			t.Fatal(err)
		}
		write(t, staging.ClaimPath(next), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
		zoneStore(t, h)
		defer failPlacement()()
		o := h.options()
		o.StateBackup = backupOf(h, "resume-after")
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		check(t, h, result, code, backupOf(h, "resume-after"), old)
	})
	t.Run("rollback", func(t *testing.T) {
		h, _, second, _, next := zoneInstalled(t)
		h.mustInstall(t, "update", second)
		zoneStore(t, h)
		defer failPlacement()()
		o := h.options()
		o.StateBackup = backupOf(h, "rollback-after")
		result, code := install.Rollback(context.Background(), o, "")
		check(t, h, result, code, backupOf(h, "rollback-after"), next)
	})
}
