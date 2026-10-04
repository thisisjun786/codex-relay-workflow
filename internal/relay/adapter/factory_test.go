package adapter

import (
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

func TestHostFactoryConfiguresOwnedClientOnly(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "RPC-double"}[supplied], func(t *testing.T) {
			root := t.TempDir()
			calls := 0
			f := hostFactory{configure: func(c *appserver.Client) {
				if c == nil {
					t.Fatal("no client")
				}
				calls++
			}}
			options := Options{}
			if supplied {
				options.RPC = &scriptRPC{}
			}
			a, err := f.open(filepath.Join(root, "absent.sock"), filepath.Join(root, "ledger"), options)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			want := 1
			if supplied {
				want = 0
			}
			if calls != want {
				t.Fatalf("configuration calls %d, want %d", calls, want)
			}
		})
	}
}
