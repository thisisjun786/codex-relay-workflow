package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The store's two sidecars under the state backup. SQLite keeps a write-ahead log and a shared-memory index beside the
// database while any connection is open and deletes them when the last one closes, so either can appear or go between
// the backup's first listing and its copy. The index is never copied (SQLite rebuilds it from the log on open); the log
// is copied when it is there at its copy, and a log that goes or arrives while the copy is made is not a refusal. The
// store's own file has to be what was read either way, and every other file keeps its rule.

const (
	sidecarShmName = "relay.sqlite3-shm"
	sidecarWalName = "relay.sqlite3-wal"
	shmNotCopied   = "not copied: the WAL index SQLite rebuilds from the log on open"
)

// withoutShm drops SQLite's shared-memory index from a reading of a directory, as the backup leaves it out. The
// write-ahead log stays in: a backup carries it when it was there at its copy, and the existing backup tests pin that.
func withoutShm(tree map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range tree {
		if k == sidecarShmName {
			continue
		}
		out[k] = v
	}
	return out
}

// ---- CRW-862 review fixes ----

// The store sidecars are classified by the resolved database path even when the log cannot be read in place: an
// unclean shutdown leaves a linked store with a -wal holding frames and no -shm, and InPlaceRead refuses that state,
// but the sidecars still sit beside the file the link names. Before the fix sidecarsFor fell back to the top-level
// names, so the real log was walked as an ordinary nested file and the copy could lose its committed frames.
//
// sequential: none (a white-box reading).
func TestTheSidecarsAreResolvedEvenWhenTheLogCannotBeReadInPlace(t *testing.T) {
	h, _, _, _, _ := zoneInstalled(t)
	zoneStore(t, h)
	real := filepath.Join(h.relayState, "data", "real.sqlite3")
	testsupport.Create(t, real, "", "go")
	// frames in the log, no index: InPlaceRead refuses this state
	write(t, real+"-wal", strings.Repeat("x", 64))
	if err := os.Remove(filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	wal, shm := install.SidecarsFor(h.relayState, filepath.Join(h.relayState, "relay.sqlite3"))
	if wal != real+"-wal" || shm != real+"-shm" {
		t.Fatalf("sidecars = (%q, %q), want (%q, %q): the resolved file sidecars, not the top-level names", wal, shm, real+"-wal", real+"-shm")
	}
}

// A log that goes during the SECOND listing walk is absence, not a change: the comparison must not read its empty
// vanished entry as a moved identity. Before the fix that entry identity (with no source) differed from the copied
// log identity and the backup refused, so the same checkpoint race passed or failed on timing.
//
// sequential: replaces the state-backup walk seam.
func TestTheBackupDoesNotRefuseALogThatGoesDuringTheSecondListing(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarWalName, 32)
	wal := filepath.Join(h.relayState, sidecarWalName)
	listings := 0
	restore := install.ReplaceStateBackupWalk(func(path string) error {
		if path == wal {
			listings++
			// the first listing and the copy see the log; the second listing loses it between its read and its Info
			if listings >= 2 {
				return os.Remove(path)
			}
		}
		return nil
	})
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "second-listing-gone")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("a log that goes during the second listing must not refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
}

// The integrity gate scratch duplicate is charged against the destination free space: the preflight must hold the
// backup and the duplicate at once, so a destination that fits the backup alone refuses before it copies.
//
// sequential: none (a white-box reading).
func TestTheSpacePreflightCoversTheIntegrityScratchDuplicate(t *testing.T) {
	need := install.ScratchNeed(map[string]int64{
		"relay.sqlite3":     90 << 20,
		"relay.sqlite3-wal": 4 << 20,
		"ledger.log":        1 << 20,
	})
	if want := int64(94 << 20); need != want {
		t.Fatalf("scratch need = %d, want %d (the store and its log, not the other files)", need, want)
	}
}

// ---- CRW-862: the three PR #735 P1s and the integrity gate ----

// P1 1: the store's write-ahead log is compared by its identity, not dropped from the comparison. A log that is
// itself a symbolic link and is retargeted between the two listings is a change: the copy would hold bytes from a file
// the state directory no longer names. Before the fix the whole WAL entry was dropped from listingDiff and this passed.
//
// sequential: replaces the state-backup seam.
func TestTheBackupRefusesAWALLinkRetargetedBetweenTheListings(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	// the store's log is a link to a file inside the state directory (an empty log, so the gate's own read of the
	// store still succeeds), and the link is retargeted between the first listing and the copy
	first := filepath.Join(h.relayState, "wal-one")
	secondTarget := filepath.Join(h.relayState, "wal-two")
	write(t, first, "")
	write(t, secondTarget, "")
	if err := os.Symlink(first, filepath.Join(h.relayState, sidecarWalName)); err != nil {
		t.Fatal(err)
	}
	restore := install.ReplaceStateBackupListed(func() error {
		if err := os.Remove(filepath.Join(h.relayState, sidecarWalName)); err != nil {
			return err
		}
		return os.Symlink(secondTarget, filepath.Join(h.relayState, sidecarWalName))
	})
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "wal-retargeted")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old {
		t.Fatalf("a retargeted WAL link must refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if !strings.Contains(text(at(result, "swapGate", "stateBackup", "error")), "write-ahead log changed") {
		t.Fatalf("the refusal must name the changed log: %s", golden.Canon(at(result, "swapGate", "stateBackup")))
	}
}

// P1 2: the store's sidecars are recognised by the path SQLite resolves the database to, not by the top-level name.
// With relay.sqlite3 linked to data/real.sqlite3 inside the state directory, that file's -shm going is not a refusal
// and is never listed (SQLite rebuilds it from the log on open), and its -wal follows the sidecar rules (a log that
// goes before its copy is recorded as gone, not refused). Before the fix both were walked as ordinary files, so the
// -shm vanishing refused.
//
// sequential: replaces the state-backup seam.
func TestTheBackupRecognisesTheSidecarsOfAStoreLinkedInsideTheStateDirectory(t *testing.T) {
	for name, gone := range map[string]string{
		"the index goes after the listing": "data/real.sqlite3-shm",
		"the log goes before its copy":     "data/real.sqlite3-wal",
	} {
		t.Run(name, func(t *testing.T) {
			h, _, second, _, next := zoneInstalled(t)
			zoneStore(t, h)
			real := filepath.Join(h.relayState, "data", "real.sqlite3")
			testsupport.Create(t, real, "", "go")
			putSidecar(t, h, filepath.Join("data", "real.sqlite3-shm"), 32768)
			putSidecar(t, h, filepath.Join("data", "real.sqlite3-wal"), 32)
			if err := os.Remove(filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(h.relayState, "relay.sqlite3")); err != nil {
				t.Fatal(err)
			}
			restore := install.ReplaceStateBackupListed(func() error { return os.Remove(filepath.Join(h.relayState, filepath.FromSlash(gone))) })
			defer restore()
			o := h.options()
			o.StateBackup = backupOf(h, "linked-inside")
			result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
			if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
				t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
			}
			backup := backupOf(h, "linked-inside")
			nothingAt(t, filepath.Join(backup, "data", "real.sqlite3-shm"))
		})
	}
}

// P1 3: an ENOENT from the walk's Info on the store's log is recorded as gone before its copy, not a refusal. The
// seam makes the log go between the directory read and the entry's own information. Another file removed the same way
// still refuses.
//
// sequential: replaces the state-backup walk seam.
func TestTheBackupRecordsALogThatGoesDuringTheWalk(t *testing.T) {
	t.Run("the store's log", func(t *testing.T) {
		h, _, second, _, next := zoneInstalled(t)
		zoneStore(t, h)
		putSidecar(t, h, sidecarWalName, 32)
		wal := filepath.Join(h.relayState, sidecarWalName)
		removed := false
		restore := install.ReplaceStateBackupWalk(func(path string) error {
			if path == wal && !removed {
				removed = true
				return os.Remove(path)
			}
			return nil
		})
		defer restore()
		o := h.options()
		o.StateBackup = backupOf(h, "walk-gone")
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
			t.Fatalf("a log that goes during the walk is not a refusal: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
		}
		backup := backupOf(h, "walk-gone")
		nothingAt(t, filepath.Join(backup, sidecarWalName))
		if got := storeSidecarsOf(t, backup)[sidecarWalName]; got != "gone before its copy" {
			t.Errorf("storeSidecars[%s] = %q, want %q", sidecarWalName, got, "gone before its copy")
		}
	})
	t.Run("another file", func(t *testing.T) {
		h, _, second, old, _ := zoneInstalled(t)
		zoneStore(t, h)
		ledger := filepath.Join(h.relayState, "ledger.log")
		removed := false
		restore := install.ReplaceStateBackupWalk(func(path string) error {
			if path == ledger && !removed {
				removed = true
				return os.Remove(path)
			}
			return nil
		})
		defer restore()
		o := h.options()
		o.StateBackup = backupOf(h, "walk-gone-other")
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old {
			t.Fatalf("another file's ENOENT must still refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
		}
	})
}

// The integrity gate: a copy that fails PRAGMA integrity_check is not a restore candidate. The manifest records
// integrityCheck and restoreCandidate, the command refuses, and the copy's bytes are not changed by the check (it runs
// on a scratch duplicate). A healthy copy records ok and true.
//
// sequential: replaces the state-backup integrity seam.
func TestTheBackupRecordsIntegrityAndRefusesACorruptedCopy(t *testing.T) {
	t.Run("a corrupted copy", func(t *testing.T) {
		h, _, second, old, _ := zoneInstalled(t)
		zoneStore(t, h)
		restore := install.ReplaceIntegrityCheck(func(dest string, copied []install.BackedUp) (string, error) {
			return "Page 5 is never used", errors.New("PRAGMA integrity_check answered a corruption")
		})
		defer restore()
		o := h.options()
		o.StateBackup = backupOf(h, "corrupt")
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old {
			t.Fatalf("a corrupted copy must refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
		}
		backup := backupOf(h, "corrupt")
		var manifest struct {
			IntegrityCheck   string `json:"integrityCheck"`
			RestoreCandidate bool   `json:"restoreCandidate"`
		}
		if err := json.Unmarshal(mustRead(t, backup+install.ManifestSuffix), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.RestoreCandidate || !strings.Contains(manifest.IntegrityCheck, "never used") {
			t.Fatalf("manifest: %+v", manifest)
		}
	})
	t.Run("a healthy copy", func(t *testing.T) {
		h, _, second, _, next := zoneInstalled(t)
		zoneStore(t, h)
		o := h.options()
		o.StateBackup = backupOf(h, "healthy")
		result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
		if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
			t.Fatalf("a healthy copy must pass: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
		}
		backup := backupOf(h, "healthy")
		var manifest struct {
			IntegrityCheck   string `json:"integrityCheck"`
			RestoreCandidate bool   `json:"restoreCandidate"`
		}
		if err := json.Unmarshal(mustRead(t, backup+install.ManifestSuffix), &manifest); err != nil {
			t.Fatal(err)
		}
		if !manifest.RestoreCandidate || manifest.IntegrityCheck != "ok" {
			t.Fatalf("manifest: %+v", manifest)
		}
		if !bytes.Equal(mustRead(t, filepath.Join(backup, "relay.sqlite3")), mustRead(t, filepath.Join(h.relayState, "relay.sqlite3"))) {
			t.Fatal("the integrity check changed the copy of the store")
		}
	})
}

// storeSidecarsOf is the storeSidecars record of a backup's manifest, read from the file.
func storeSidecarsOf(t *testing.T, backup string) map[string]string {
	t.Helper()
	var manifest struct {
		StoreSidecars map[string]string `json:"storeSidecars"`
	}
	if err := json.Unmarshal(mustRead(t, backup+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.StoreSidecars
}

// putSidecar writes a sidecar the way SQLite leaves one beside a store: an index of its own size, or a log that holds
// no frame.
func putSidecar(t *testing.T, h *host, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.relayState, name), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An index that goes between the listing and the copy is not a refusal: it is never copied, so the backup is complete
// without it.
//
// sequential: replaces the state-backup seams.
func TestTheBackupDoesNotRefuseAnIndexThatGoesAfterTheListing(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarShmName, 32768)
	putSidecar(t, h, sidecarWalName, 0)
	shm := filepath.Join(h.relayState, sidecarShmName)
	restore := install.ReplaceStateBackupListed(func() error { return os.Remove(shm) })
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "shm-gone")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	backup := backupOf(h, "shm-gone")
	nothingAt(t, filepath.Join(backup, sidecarShmName))
	if got := storeSidecarsOf(t, backup)[sidecarShmName]; got != shmNotCopied {
		t.Errorf("storeSidecars[%s] = %q, want %q", sidecarShmName, got, shmNotCopied)
	}
}

// An index that goes after the copy is not a refusal either: the comparison leaves both sidecars out.
//
// sequential: replaces the state-backup seams.
func TestTheBackupDoesNotRefuseAnIndexThatGoesAfterTheCopy(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarShmName, 32768)
	putSidecar(t, h, sidecarWalName, 0)
	shm := filepath.Join(h.relayState, sidecarShmName)
	restore := install.ReplaceStateBackupStep(func(step string) error {
		if step == "copied" {
			return os.Remove(shm)
		}
		return nil
	})
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "shm-gone-after")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	nothingAt(t, filepath.Join(backupOf(h, "shm-gone-after"), sidecarShmName))
}

// A log that is checkpointed away between the listing and its copy is dropped, and the manifest says so.
//
// sequential: replaces the state-backup seams.
func TestTheBackupDropsALogThatGoesBeforeItsCopy(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarWalName, 32)
	wal := filepath.Join(h.relayState, sidecarWalName)
	restore := install.ReplaceStateBackupListed(func() error { return os.Remove(wal) })
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "wal-gone")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	backup := backupOf(h, "wal-gone")
	nothingAt(t, filepath.Join(backup, sidecarWalName))
	if got := storeSidecarsOf(t, backup)[sidecarWalName]; got != "gone before its copy" {
		t.Errorf("storeSidecars[%s] = %q, want %q", sidecarWalName, got, "gone before its copy")
	}
}

// The store's own file and every other file keep the rule they had: a copy that is not of one moment still refuses.
//
// sequential: replaces the state-backup seams.
func TestTheBackupStillRefusesWhatChangedOtherwise(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, h *host)
		step  func(t *testing.T, h *host, step string) error
		want  string
	}{
		"a copied log grows before the verification": {
			setup: func(t *testing.T, h *host) { putSidecar(t, h, sidecarWalName, 32) },
			step: func(t *testing.T, h *host, step string) error {
				if step != "copied" {
					return nil
				}
				return appendTo(filepath.Join(h.relayState, sidecarWalName), strings.Repeat("x", 32))
			},
			want: "changed",
		},
		"the store grows": {
			setup: func(t *testing.T, h *host) {},
			step: func(t *testing.T, h *host, step string) error {
				if step != "copied" {
					return nil
				}
				return appendTo(filepath.Join(h.relayState, "relay.sqlite3"), "x")
			},
			want: "changed",
		},
		"another file goes": {
			setup: func(t *testing.T, h *host) {},
			step: func(t *testing.T, h *host, step string) error {
				if step != "listed" {
					return nil
				}
				return os.Remove(filepath.Join(h.relayState, "ledger.log"))
			},
			want: "could not be copied",
		},
		"another file changes size between the listing and its copy": {
			setup: func(t *testing.T, h *host) {},
			step: func(t *testing.T, h *host, step string) error {
				if step != "listed" {
					return nil
				}
				// its bytes are copied as they are now, but its size in the comparison stays the one the first listing
				// recorded, so a file that changed size under the copy is still the change it always was
				return os.WriteFile(filepath.Join(h.relayState, "ledger.log"), []byte("line one\n"), 0o640)
			},
			want: "changed",
		},
		"a log that was not copied holds commits in the second listing": {
			setup: func(t *testing.T, h *host) {},
			step: func(t *testing.T, h *host, step string) error {
				switch step {
				case "listed":
					// the empty log the gate's own read left is deleted before its copy, so it is dropped
					return os.Remove(filepath.Join(h.relayState, sidecarWalName))
				case "copied":
					// and another connection commits into a fresh log before the second listing: a commit that stays
					// in the log does not touch relay.sqlite3 until a checkpoint, so the digest check cannot see it
					return os.WriteFile(filepath.Join(h.relayState, sidecarWalName), make([]byte, 64), 0o600)
				}
				return nil
			},
			want: "holds commits that were not copied",
		},
		"a copied log whose source goes and whose copy is damaged": {
			setup: func(t *testing.T, h *host) { putSidecar(t, h, sidecarWalName, 32) },
			step: func(t *testing.T, h *host, step string) error {
				if step != "copied" {
					return nil
				}
				// the log is checkpointed away, so its source cannot be compared any more, and the copy in the
				// backup is damaged at the same time: the copy must still be read, or a bad backup passes
				if err := os.Remove(filepath.Join(h.relayState, sidecarWalName)); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(backupOf(h, "changed"), sidecarWalName), []byte("damaged"), 0o600)
			},
			want: "is not the file it copies",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, second, old, _ := zoneInstalled(t)
			zoneStore(t, h)
			tc.setup(t, h)
			restoreListed := install.ReplaceStateBackupListed(func() error { return tc.step(t, h, "listed") })
			defer restoreListed()
			restoreStep := install.ReplaceStateBackupStep(func(step string) error { return tc.step(t, h, step) })
			defer restoreStep()
			o := h.options()
			o.StateBackup = backupOf(h, "changed")
			result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
			if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old ||
				!strings.Contains(text(at(result, "swapGate", "stateBackup", "error")), tc.want) {
				t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
			}
		})
	}
}

// The branch a log takes when it is absent from the first listing and appears in the second cannot be reached end to
// end: the swap gate's own read of the store leaves an empty relay.sqlite3-wal in the state directory before the
// backup's first listing, so the listing always holds a -wal. It is pinned here, white-box, against the two pieces the
// end-to-end path uses: the comparison of two listings (which leaves both sidecars out, so the appearance is not a
// change) and the record that reading produces for the manifest.
//
// sequential: it pins the state-backup sidecar record, which the end-to-end tests of this file also exercise.
func TestTheAppearedLogBranchIsNotACopyAndNotAChange(t *testing.T) {
	// an -shm going or arriving is not a change either way, and neither is a -wal: both names are out of the comparison
	for name, tc := range map[string]struct {
		first, second []string
	}{
		"a log appears":    {[]string{"relay.sqlite3", "ledger.log"}, []string{"relay.sqlite3", "ledger.log", sidecarWalName}},
		"a log goes":       {[]string{"relay.sqlite3", "ledger.log", sidecarWalName}, []string{"relay.sqlite3", "ledger.log"}},
		"an index appears": {[]string{"relay.sqlite3"}, []string{"relay.sqlite3", sidecarShmName}},
		"an index goes":    {[]string{"relay.sqlite3", sidecarShmName}, []string{"relay.sqlite3"}},
	} {
		t.Run(name, func(t *testing.T) {
			if diff := install.ListingDiff(tc.first, tc.second); diff != "" {
				t.Errorf("the sidecars must be out of the comparison, got %q", diff)
			}
		})
	}
	// another file's appearance or departure is still a change: the exclusion is the two sidecar names only
	if diff := install.ListingDiff([]string{"relay.sqlite3"}, []string{"relay.sqlite3", "ledger.log"}); diff == "" {
		t.Error("another file appearing must still be a change")
	}
	if diff := install.ListingDiff([]string{"relay.sqlite3", "ledger.log"}, []string{"relay.sqlite3"}); diff == "" {
		t.Error("another file going must still be a change")
	}
	// and relay.sqlite3 itself is not excluded: a store that changed under the copy is still a change
	if diff := install.ListingDiff([]string{"relay.sqlite3"}, []string{"relay.sqlite3-wal"}); diff == "" {
		t.Error("the store's own file going must still be a change")
	}

	// the record of a log absent from the first listing and present in the second, never copied
	if got := install.StoreSidecarWal(false, false, true); got != "appeared after the listing, not copied" {
		t.Errorf("appeared: %q", got)
	}
	// the three readings the end-to-end path reaches, so the record is pinned whole
	for want, flags := range map[string][3]bool{
		"copied":               {true, false, false},
		"gone before its copy": {false, true, false},
		"absent":               {false, false, false},
	} {
		if got := install.StoreSidecarWal(flags[0], flags[1], flags[2]); got != want {
			t.Errorf("flags %v: %q, want %q", flags, got, want)
		}
	}
}
