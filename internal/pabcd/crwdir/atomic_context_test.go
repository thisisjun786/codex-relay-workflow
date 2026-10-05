package crwdir

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A cancellation that lands at the rename step, after the write and the sync, is reported by the
// step hook PublishContext installs: the deferred cleanup removes the temp file and finalPath is
// left as it was.
func TestPublishContextCancelAtTheRenameStepLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "test-receipt.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := publishContext(ctx, final, []byte("receipt"), func(at publishStep) error {
		if at == stepRename {
			if got := len(temps(t, dir)); got != 1 {
				t.Errorf("%d temp files at the rename step, want 1", got)
			}
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(final); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the final file exists: %v", statErr)
	}
	if left := names(t, dir); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// The same refusal through the exported entry point, with the context cancelled before the call.
func TestPublishContextAlreadyCancelledLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "test-receipt.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PublishContext(ctx, final, []byte("receipt")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(final); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the final file exists: %v", statErr)
	}
	if left := names(t, dir); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// An uncancelled PublishContext writes what Publish writes, byte for byte and mode for mode.
func TestPublishContextUncancelledMatchesPublish(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	leftFinal := filepath.Join(left, "test-receipt.json")
	rightFinal := filepath.Join(right, "test-receipt.json")
	for _, path := range []string{leftFinal, rightFinal} {
		if err := os.WriteFile(path, []byte("old"), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("{\"kind\":\"test\"}\n")
	if err := PublishContext(context.Background(), leftFinal, data); err != nil {
		t.Fatalf("PublishContext: %v", err)
	}
	if err := Publish(rightFinal, data); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if read(t, leftFinal) != read(t, rightFinal) || modeOf(t, leftFinal) != modeOf(t, rightFinal) {
		t.Errorf("bytes %q vs %q, mode %v vs %v", read(t, leftFinal), read(t, rightFinal), modeOf(t, leftFinal), modeOf(t, rightFinal))
	}
	if !slices.Equal(names(t, left), names(t, right)) {
		t.Errorf("directories %v vs %v", names(t, left), names(t, right))
	}
}
