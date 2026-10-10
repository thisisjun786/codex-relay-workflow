package dagsched

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// goexitTB is a testing.TB whose Fatalf ends the calling goroutine the way a failing test's does, without failing the real test.
type goexitTB struct {
	testing.TB
}

func (goexitTB) Helper()               {}
func (goexitTB) Name() string          { return "TestFirstFixture" }
func (goexitTB) Fatalf(string, ...any) { runtime.Goexit() }

// A template whose first build fails must hand every later caller that failure, not the empty directory with a nil error.
func TestStoreTemplateBuildFailureIsRecordedForLaterCallers(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "store-template") // a directory under a regular file: Create cannot make the store
	var b storeTemplateBuild
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.get(goexitTB{t}, dir) // Goexits at Create
	}()
	<-done
	if err := b.get(t, dir); err == nil {
		t.Fatal("a later caller got a nil error after the first build failed")
	}
}
