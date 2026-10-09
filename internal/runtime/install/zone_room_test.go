package install_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// CRW-837. The room check of the state backup rests on the first listing of the state directory. The store's log is a file another connection checkpoints away
// at any moment, so a large log that is gone by the time the check refuses made the need smaller than the listing said. The state directory is read once more
// before any byte is copied, and the check is made again only when that listing needs less.

// A log the first listing held and the second does not: the second listing's need is judged, the backup is made, and the manifest says the log went.
//
// sequential: replaces the state-backup room seam and listed seam.
func TestTheBackupJudgesTheRoomAgainWhenTheLogHasGone(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarWalName, 32)
	wal := filepath.Join(h.relayState, sidecarWalName)
	var needs []int64
	restoreRoom := install.ReplaceStateBackupRoom(func(parent string, need int64) error {
		needs = append(needs, need)
		if len(needs) == 1 {
			return errors.New("the first listing would not fit")
		}
		return nil
	})
	defer restoreRoom()
	restoreListed := install.ReplaceStateBackupListed(func() error { return os.Remove(wal) })
	defer restoreListed()
	o := h.options()
	o.StateBackup = backupOf(h, "room-wal-gone")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("a log that went before the room was judged again must not refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if len(needs) != 2 || needs[1] >= needs[0] {
		t.Fatalf("the room was judged for needs %v, want the first listing's and then a smaller one", needs)
	}
	backup := backupOf(h, "room-wal-gone")
	nothingAt(t, filepath.Join(backup, sidecarWalName))
	if got := storeSidecarsOf(t, backup)[sidecarWalName]; got != "gone before its copy" {
		t.Errorf("storeSidecars[%s] = %q, want %q", sidecarWalName, got, "gone before its copy")
	}
}

// A log that is still there refuses on the first listing's need, as before: the second reading needs no less, the room is not judged again, and nothing is
// created.
//
// sequential: replaces the state-backup room seam.
func TestTheBackupRefusesOnTheRoomWhenTheLogIsStillThere(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarWalName, 32)
	calls := 0
	restoreRoom := install.ReplaceStateBackupRoom(func(parent string, need int64) error {
		calls++
		return errors.New("the state directory would not fit")
	})
	defer restoreRoom()
	o := h.options()
	o.StateBackup = backupOf(h, "room-wal-stays")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old {
		t.Fatalf("a refusal on the room must stand: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if calls != 1 {
		t.Fatalf("the room was judged %d times, want once: the second listing needs no less", calls)
	}
	nothingAt(t, backupOf(h, "room-wal-stays"))
}
