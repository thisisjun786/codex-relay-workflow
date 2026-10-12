package trace_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

// TestRefuseLiveReadsTheRawSpelling (CRW-1208 after the evaluation, d1): a relay_state spelled through a symbolic link and
// ".." names the directory the filesystem gives that spelling, and the relay keeps the spelling. The live-store refusal reads the
// configured root and the path it judges the same way, so the store at the real place is refused whatever the output is spelled
// like, a path in the directory the lexically cleaned spelling would name is not, and a report written to a ".."-spelled path
// lands where the kernel puts it and is judged there.
func TestRefuseLiveReadsTheRawSpelling(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	data, err := os.ReadFile(s.Copy())
	if err != nil {
		t.Fatal(err)
	}
	// a short directory: a store socket below the configured root must fit a unix socket path
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"a", "b/deep", "b/rs/0123456789abcdef", "b/elsewhere", "a/rs"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "b", "deep"), filepath.Join(base, "a", "link")); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(base, "b", "rs", "0123456789abcdef", "relay.sqlite3")
	if err := os.WriteFile(live, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(base, "config.json")
	if err := os.WriteFile(config, []byte(`{"paths":{"relay_state":"`+base+`/a/link/../rs"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	if got, err := crwconfig.ConfiguredRelayState(os.Getenv); err != nil || got != base+"/a/link/../rs" {
		t.Fatalf("the configured relay state = %q, %v", got, err)
	}

	// the live file, in its real spelling and in a spelling that reaches it through the link
	for name, p := range map[string]string{
		"the real path":               live,
		"a new file beside it":        filepath.Join(base, "b", "rs", "0123456789abcdef", "report.json"),
		"through the link and ..":     base + "/a/link/../rs/0123456789abcdef/relay.sqlite3",
		"a new file through the link": base + "/a/link/../rs/0123456789abcdef/report.json",
	} {
		if err := trace.RefuseLive(p); !errors.Is(err, trace.ErrLiveStore) {
			t.Errorf("%s: RefuseLive(%s) = %v, want ErrLiveStore", name, p, err)
		}
	}
	// the directory the cleaned spelling names (a/rs) is no live state, and a path that ".." leads out of the live tree is not
	for name, p := range map[string]string{
		"the cleaned spelling's directory": filepath.Join(base, "a", "rs", "report.json"),
		"a sibling reached through ..":     base + "/a/link/../elsewhere/report.json",
	} {
		if err := trace.RefuseLive(p); err != nil {
			t.Errorf("%s: RefuseLive(%s) = %v, want nil", name, p, err)
		}
	}
	// the write goes where the kernel resolves the spelling, and the live database is never replaced
	before, _ := os.ReadFile(live)
	if err := trace.WriteBytes(live, []byte("{}\n")); !errors.Is(err, trace.ErrLiveStore) {
		t.Errorf("WriteBytes(live) = %v", err)
	}
	if err := trace.WriteBytes(base+"/a/link/../rs/0123456789abcdef/relay.sqlite3", []byte("{}\n")); !errors.Is(err, trace.ErrLiveStore) {
		t.Errorf("WriteBytes(through the link) = %v", err)
	}
	if after, _ := os.ReadFile(live); string(after) != string(before) {
		t.Errorf("the live database changed")
	}
	if err := trace.WriteBytes(base+"/a/link/../elsewhere/report.json", []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "b", "elsewhere", "report.json")); err != nil {
		t.Errorf("the report is not where the spelling leads: %v", err)
	}
}
