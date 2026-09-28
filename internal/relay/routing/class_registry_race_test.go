package routing

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

func Test23ClassRegistryConcurrentRouterConstruction(t *testing.T) {
	faults.InstallProductDeclarations()
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				New(nil, nil)
				if err := faults.RegisterClass("completion_mismatch", 1); err != nil {
					t.Error(err)
				}
				if err := faults.RegisterClassThreshold("completion_mismatch", 1); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !faults.RegisteredClass("completion_mismatch") {
					t.Error("class disappeared")
				}
			}
		}()
		dir := t.TempDir()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				faults.ExecuteAs(context.Background(), "codex-session-relay", []string{"--state", dir, "fault-policy", "--product", "v"}, io.Discard, io.Discard, nil)
			}
		}()
	}
	wg.Wait()
}
