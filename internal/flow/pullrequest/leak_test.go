package pullrequest

import (
	"strings"
	"testing"
)

func TestBodyRefusesAHomeStateOrScratchPathAHostnameAndAThreadId(t *testing.T) {
	ctx := LeakContext{Hostname: "lina-os", Home: "/root/someone"}
	refuse := []struct {
		name, text, kind string
	}{
		{"home at the start of the text", "/home/u/x is the file", "path"},
		{"state at the start of a later line", "Result\n/state/crw/x\nmore", "path"},
		{"state after a space", "see /state/x now", "path"},
		{"scratch in backticks", "run `/scratch/y` first", "path"},
		{"home in parentheses", "(/home/z)", "path"},
		{"state in quotes", `the "/state/q" dir`, "path"},
		{"home in a file url", "file:///home/a", "path"},
		{"scratch after an assignment", "x=/scratch/b", "path"},
		{"bare home at the end of a line", "cd /home\nnext", "path"},
		{"home in angle brackets", "<https://x/y> </home/k>", "path"},
		{"the home value", "from /root/someone/work", "path"},
		{"the stub hostname", "built on lina-os today", "hostname"},
		{"the hostname in other case", "built on LINA-OS.", "hostname"},
		{"a thread id", "thread 0190a1b2-c3d4-4e5f-8a9b-0123456789ab here", "thread_id"},
		{"an engine call id", "see c_0123456789ab", "call_id"},
		{"a worktree call name", "dir call-c_0123456789ab", "call_id"},
		{"a host call id", "id call_" + strings.Repeat("a1B2", 6), "call_id"},
		{"a tool call id", "id toolu_" + strings.Repeat("Ab3d", 5), "call_id"},
		{"the flow op literal", "crw-flow-op: x", "call_id"},
		{"the handoff literal", "token crwh1:abc", "call_id"},
		{"a run id", "run-0123456789ab finished", "run_id"},
		{"a request id", "req_" + strings.Repeat("0a", 12), "run_id"},
		{"a report id", "rep_" + strings.Repeat("f1", 12), "run_id"},
		{"a secret", "token ghp_" + strings.Repeat("A", 24), "secret"},
		{"a run of three assignments", "Env:\nA=1\nB=2\nC=3\nend", "secret"},
		{"a secret name in json in a code fence", "Output:\n```json\n{\n  \"value\": \"TOKEN=private-value\"\n}\n```\nend", "secret"},
		{"home in bold", "at **/root/someone**", "path"},
		{"a secret name in multi-line json", "{\n  \"value\": \"TOKEN=private-value\"\n}", "secret"},
	}
	for _, tc := range refuse {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			got := Leaks(tc.text, ctx)
			if len(got) == 0 {
				t.Fatalf("Leaks(%q) = none, want a %s leak", tc.text, tc.kind)
			}
			found := false
			for _, l := range got {
				if l.Kind == tc.kind {
					found = true
				}
				if l.Match == "" {
					t.Fatalf("a leak of kind %s has no match: %+v", l.Kind, got)
				}
			}
			if !found {
				t.Fatalf("Leaks(%q) = %+v, want kind %s", tc.text, got, tc.kind)
			}
		})
	}
	clean := `## Result
Milestone M1 closed at 0123456789ab; gate passed.
## Closes
- thisisjun786/codex-relay-workflow#1440
- node key thisisjun786:codex-relay-workflow:1440 for CRW-1440
## Verification
- test go test ./internal/flow/pullrequest/... passed 0123456789ab
## Crosscheck
verified at 0123456789abcdef0123456789abcdef01234567, round 1
docs/state/x.md and a/home/b and .../home/ and src/scratch/z are relative.
The call-sites and call_id words, a lina-os-2 host, /homework and /statement are not leaks.
Visit https://example.com/home/x or /tmp/work.`
	for name, c := range map[string]LeakContext{"stub": ctx, "empty": {}, "localhost": {Hostname: "LocalHost", Home: "/"}} {
		t.Run("passes a clean body with "+name+" context", func(t *testing.T) {
			if got := Leaks(clean, c); len(got) != 0 {
				t.Fatalf("Leaks(clean) = %+v, want none", got)
			}
		})
	}
}

func TestLeakNamesTheKindAndOnlyAPrefixOfTheMatch(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 24)
	got := Leaks("a /home/someone/very/long/private/path and "+secret, LeakContext{})
	if len(got) != 2 {
		t.Fatalf("Leaks = %+v, want a path and a secret", got)
	}
	for _, l := range got {
		s := l.Short()
		if !strings.HasPrefix(s, l.Kind+" ") {
			t.Errorf("Short() = %q, want it to start with the kind %q", s, l.Kind)
		}
		if strings.Contains(s, "private/path") || strings.Contains(s, secret) {
			t.Errorf("Short() = %q echoes a whole match", s)
		}
	}
	if l := got[0]; l.Kind != "path" || !strings.HasPrefix(l.Match, "/home/someone") {
		t.Errorf("first leak = %+v, want the path", l)
	}
}

func TestLeakHomeDirectoryNeedsAPathBoundary(t *testing.T) {
	c := LeakContext{Home: "/root/"}
	for _, text := range []string{"docs/root.md and internal/root.go", "see /rooted/x and a/root/b", "x/root", "the /rootfs"} {
		if got := Leaks(text, c); len(got) != 0 {
			t.Errorf("Leaks(%q) = %+v, want none", text, got)
		}
	}
	for _, text := range []string{"/root/work/x", "cd (/root)", "at /root", "p=/root/a b"} {
		got := Leaks(text, c)
		if len(got) != 1 || got[0].Kind != LeakPath || !strings.HasPrefix(got[0].Match, "/root") {
			t.Errorf("Leaks(%q) = %+v, want one /root path leak", text, got)
		}
	}
}
