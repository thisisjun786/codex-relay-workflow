package hook

import (
	"os"
	"testing"
	"time"
)

// TestAutomationReadOwnershipRefusesAChangeThatKeepsSizeAndModTime: the oracle compares ctimeMs between the
// fstat before the read and the one after it (automation-store.ts:124), so a rewrite of the same length
// whose mtime was put back, and a metadata-only change (chmod), both make ownership unavailable (CRW-804).
// The change lands after the bytes were read and before the second fstat.
func TestAutomationReadOwnershipRefusesAChangeThatKeepsSizeAndModTime(t *testing.T) {
	mutations := map[string]func(t *testing.T, path string){
		"chmod": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"same length rewrite with the mtime restored": func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)-2] ^= 0x01 // one byte of the same length
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fixture := automationNewFixture(t, automationStoreText(automationOwner))
			t.Cleanup(func() { automationAfterRead = nil })
			automationAfterRead = func(path string) {
				time.Sleep(20 * time.Millisecond) // the ctime of the change differs from the one before it on any clock
				mutate(t, path)
			}
			if snapshot, err := automationReadOwnership(fixture.home, automationID); err == nil {
				t.Fatalf("a store changed during the read was accepted: %+v", snapshot)
			}
		})
	}
	t.Run("an untouched store still reads", func(t *testing.T) {
		fixture := automationNewFixture(t, automationStoreText(automationOwner))
		snapshot, err := automationReadOwnership(fixture.home, automationID)
		if err != nil || snapshot == nil || snapshot.TargetThreadID != automationOwner {
			t.Fatalf("snapshot %+v, err %v", snapshot, err)
		}
	})
}
