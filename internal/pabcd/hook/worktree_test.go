package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// wtRig is a Codex-app managed worktree as the oracle's test builds it: <home>/.codex/worktrees/7627/opencodex
// holding a .git gitfile. The paths are real (symlink-free), because detection canonicalizes.
type wtRig struct{ home, codexHome, worktrees, slotRoot, checkout string }

func newWtRig(t *testing.T) wtRig {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := wtRig{home: home, codexHome: filepath.Join(home, ".codex")}
	r.worktrees = filepath.Join(r.codexHome, "worktrees")
	r.slotRoot = filepath.Join(r.worktrees, "7627")
	r.checkout = filepath.Join(r.slotRoot, "opencodex")
	wtWrite(t, filepath.Join(r.checkout, ".git"), "gitdir: /fake/main/.git/worktrees/7627\n")
	return r
}

func wtWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wtLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func wtEnv(m map[string]string) host.LookupEnv {
	return func(key string) (string, bool) { v, ok := m[key]; return v, ok }
}

func (r wtRig) env() host.LookupEnv {
	return wtEnv(map[string]string{"HOME": r.home, "CODEX_HOME": r.codexHome})
}

func wtGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (or cannot be read): %v", path, err)
	}
}

func wtPayload(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (r wtRig) prompt(t *testing.T, cwd, session, prompt string) (string, string) {
	t.Helper()
	fields := map[string]any{"hook_event_name": "UserPromptSubmit", "cwd": cwd, "prompt": prompt}
	if session != "" {
		fields["session_id"] = session
	}
	return HandleWorktreeGuard(wtPayload(t, fields), r.env())
}

// plain drops the canonical cwd an identity remembers for the marker, which the oracle's value has no counterpart of.
func plain(id WorktreeIdentity) WorktreeIdentity { id.cwd = ""; return id }

// The detection tests of the oracle (worktree-guard.test.ts, section 1).
func TestDetectManagedWorktree(t *testing.T) {
	r := newWtRig(t)
	nested := filepath.Join(r.checkout, "src", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	want := WorktreeIdentity{Managed: true, WorktreesDir: r.worktrees, Slot: "7627", SlotRoot: r.slotRoot, CheckoutRoot: r.checkout}

	t.Run("cwd under the default root splits into slot and checkout", func(t *testing.T) {
		if got := detectManagedWorktree(nested, r.env()); plain(got) != want || got.cwd != nested {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("CODEX_HOME decides the default root", func(t *testing.T) {
		if got := detectManagedWorktree(r.checkout, wtEnv(map[string]string{"CODEX_HOME": r.codexHome})); plain(got) != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		other := wtEnv(map[string]string{"CODEX_HOME": filepath.Join(r.home, "elsewhere")})
		if got := detectManagedWorktree(r.checkout, other); got.Managed {
			t.Fatalf("a different CODEX_HOME still covers the path: %+v", got)
		}
	})
	t.Run("CRW_WORKTREE_ROOTS adds a root", func(t *testing.T) {
		extraRoot := filepath.Join(r.home, "custom-root")
		extraCheckout := filepath.Join(extraRoot, "ab12", "repo")
		wtWrite(t, filepath.Join(extraCheckout, ".git"), "gitdir: /fake\n")
		env := wtEnv(map[string]string{
			"HOME":               r.home,
			"CODEX_HOME":         filepath.Join(r.home, "nowhere"),
			"CRW_WORKTREE_ROOTS": filepath.Join(r.home, "unused") + ":" + extraRoot,
		})
		got := detectManagedWorktree(extraCheckout, env)
		if !got.Managed || got.Slot != "ab12" || got.CheckoutRoot != extraCheckout || got.WorktreesDir != extraRoot {
			t.Fatalf("extra root not honored: %+v", got)
		}
		roots, err := candidateWorktreeRoots(env)
		wantRoots := []string{filepath.Join(r.home, "nowhere", "worktrees"), filepath.Join(r.home, "unused"), extraRoot}
		if err != nil || !slices.Equal(roots, wantRoots) {
			t.Fatalf("roots = %q, %v; want %q", roots, err, wantRoots)
		}
	})
	t.Run("candidate roots", func(t *testing.T) {
		for _, c := range []struct {
			name string
			env  map[string]string
			want []string
		}{
			{"one POSIX entry survives whole", map[string]string{"CODEX_HOME": "/x", "CRW_WORKTREE_ROOTS": "/opt/wt"}, []string{"/x/worktrees", "/opt/wt"}},
			{"entries are trimmed and empty ones dropped", map[string]string{"CODEX_HOME": " /x ", "CRW_WORKTREE_ROOTS": "/a: :/b::c"}, []string{"/x/worktrees", "/a", "/b", "c"}},
			{"a blank CODEX_HOME falls back to HOME/.codex", map[string]string{"HOME": "/h", "CODEX_HOME": "  "}, []string{"/h/.codex/worktrees"}},
			{"an empty list adds nothing", map[string]string{"CODEX_HOME": "/x", "CRW_WORKTREE_ROOTS": ""}, []string{"/x/worktrees"}},
		} {
			got, err := candidateWorktreeRoots(wtEnv(c.env))
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
			}
		}
	})
	t.Run("the root itself and unrelated paths are not managed", func(t *testing.T) {
		for _, cwd := range []string{r.worktrees, r.home, "/tmp", "", "   "} {
			if got := detectManagedWorktree(cwd, r.env()); got.Managed {
				t.Errorf("%q is managed: %+v", cwd, got)
			}
		}
	})
	t.Run("no .git up to the slot root leaves the checkout unconfirmed", func(t *testing.T) {
		r := newWtRig(t)
		if err := os.Remove(filepath.Join(r.checkout, ".git")); err != nil {
			t.Fatal(err)
		}
		got := detectManagedWorktree(r.checkout, r.env())
		if !got.Managed || got.SlotRoot != r.slotRoot || got.CheckoutRoot != "" {
			t.Fatalf("got %+v", got)
		}
		if ctx := buildSessionStartContext(got, r.checkout); !strings.Contains(ctx, "unconfirmed") || strings.Contains(ctx, "git switch") {
			t.Fatalf("context for an unconfirmed checkout: %q", ctx)
		}
	})
	t.Run("a .git at the slot root makes the slot the checkout", func(t *testing.T) {
		r := newWtRig(t)
		if err := os.Remove(filepath.Join(r.checkout, ".git")); err != nil {
			t.Fatal(err)
		}
		wtWrite(t, filepath.Join(r.slotRoot, ".git"), "gitdir: /fake\n")
		if got := detectManagedWorktree(r.checkout, r.env()); got.CheckoutRoot != r.slotRoot {
			t.Fatalf("got %+v", got)
		}
	})
}

// The canonicalization tests of the oracle (section 2) and the pins of its walk-up.
func TestCanonicalize(t *testing.T) {
	r := newWtRig(t)
	t.Run("a symlinked cwd resolves into the managed path", func(t *testing.T) {
		link := filepath.Join(r.home, "link-checkout")
		wtLink(t, r.checkout, link)
		got := detectManagedWorktree(link, r.env())
		if !got.Managed || got.CheckoutRoot != r.checkout {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a link pointing out of the slot is not managed", func(t *testing.T) {
		outside := t.TempDir()
		wtLink(t, outside, filepath.Join(r.slotRoot, "escape"))
		if got := detectManagedWorktree(filepath.Join(r.slotRoot, "escape", "x"), r.env()); got.Managed {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a missing target resolves through its nearest existing ancestor", func(t *testing.T) {
		alias := filepath.Join(r.home, "alias")
		wtLink(t, r.slotRoot, alias)
		if got, want := canonicalize(filepath.Join(r.slotRoot, "no-such", "path")), filepath.Join(r.slotRoot, "no-such", "path"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if got, want := canonicalize(filepath.Join(alias, "no-such", "path")), filepath.Join(r.slotRoot, "no-such", "path"); got != want {
			t.Errorf("through a link: got %q, want %q", got, want)
		}
	})
	t.Run("the walk-up gives up after 65 missing levels", func(t *testing.T) {
		alias := filepath.Join(r.home, "alias65")
		wtLink(t, r.slotRoot, alias)
		deep := func(n int) string { return alias + strings.Repeat("/m", n) }
		if got, want := canonicalize(deep(65)), r.slotRoot+strings.Repeat("/m", 65); got != want {
			t.Errorf("65 missing levels: got %q, want %q", got, want)
		}
		if got, want := canonicalize(deep(66)), deep(66); got != want {
			t.Errorf("66 missing levels: got %q, want the lexical path %q", got, want)
		}
	})
	t.Run("a first missing segment under the filesystem root loses its first character (oracle defect, kept)", func(t *testing.T) {
		if got := canonicalize("/crw198-no-such-top"); got != "/rw198-no-such-top" {
			t.Errorf("got %q", got)
		}
		if got := canonicalize("/crw198-no-such-top/worktrees"); got != "/rw198-no-such-top/worktrees" {
			t.Errorf("got %q", got)
		}
		if got := canonicalize("/한글-no-such-top"); got != "/글-no-such-top" {
			t.Errorf("a BMP character: got %q", got)
		}
		if got := canonicalize("/"); got != "/" {
			t.Errorf("got %q", got)
		}
	})
}

func TestDetectRenameIntent(t *testing.T) {
	for _, c := range []struct {
		prompt string
		want   bool
	}{
		{"워크트리 이름 바꾸고 싶어", true},
		{"rename the worktree please", true},
		{"워크트리에서 계속 작업해", false},
		{"이름 짓자", false},
		{"", false},
		{"RENAME THE WORKTREE", true},
		{"WorKtree re-name", true},
		{"worktree re--name", false},
		{"wor\u212atree rename", false}, // the Kelvin sign does not fold to k in a JavaScript regexp without /u
		{"worktree 명명", true},
		{"worktree 바꿔", true},
		{"worktree 지어", true},
		{"worktree 짓", true},
	} {
		if got := detectRenameIntent(c.prompt); got != c.want {
			t.Errorf("detectRenameIntent(%q) = %v, want %v", c.prompt, got, c.want)
		}
	}
}

// Section 3 of the oracle's tests: the SessionStart context.
func TestSessionStartContext(t *testing.T) {
	r := newWtRig(t)
	event, ctx := HandleWorktreeGuard(wtPayload(t, map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "cwd": r.checkout}), r.env())
	if event != "SessionStart" {
		t.Fatalf("event %q", event)
	}
	for _, want := range []string{
		"[crw: MANAGED WORKTREE — identity guard (WORKTREE-GUARD-01)]",
		"This session runs inside a Codex-app-managed worktree: " + r.checkout,
		"(cwd: " + r.checkout + "; slot: " + r.slotRoot + "; worktrees root: " + r.worktrees + ").",
		"ADOPT IN PLACE",
		"CRW_WORKTREE_ROOTS",
		"$crw:crw-worktree-guardian",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context lacks %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "codexclaw") || strings.Contains(ctx, "cxc") {
		t.Errorf("context keeps a CXC name:\n%s", ctx)
	}
	t.Run("an unmanaged cwd is silent", func(t *testing.T) {
		if _, ctx := HandleWorktreeGuard(wtPayload(t, map[string]any{"hook_event_name": "SessionStart", "session_id": "s2", "cwd": "/tmp"}), r.env()); ctx != "" {
			t.Fatalf("got %q", ctx)
		}
	})
}

var markerJSON = regexp.MustCompile(`^\{"injectedAt":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z","slot":"7627"\}$`)

// Section 4: the rename guidance once per session, through its marker.
func TestUserPromptSubmitRenameGuidance(t *testing.T) {
	r := newWtRig(t)
	const korean = "워크트리 이름 바꾸고 시작하자"
	event, first := r.prompt(t, r.checkout, "019fcd00-0000-7000-8000-000000000003", korean)
	if event != "UserPromptSubmit" || !strings.Contains(first, "[crw: MANAGED WORKTREE — rename/adopt guidance (WORKTREE-GUARD-02)]") ||
		!strings.Contains(first, "Slot protected by the PreToolUse guard: "+r.slotRoot+".") {
		t.Fatalf("first answer: %q %q", event, first)
	}
	marker := filepath.Join(r.checkout, ".crw", "worktree-guard", "019fcd00-0000-7000-8000-000000000003.json")
	data, err := os.ReadFile(marker)
	if err != nil || !markerJSON.Match(data) {
		t.Fatalf("marker %q: %v", data, err)
	}
	if info, _ := os.Stat(marker); info.Mode().Perm() != 0o600 {
		t.Errorf("marker mode %v", info.Mode().Perm())
	}
	if ignore, err := os.ReadFile(filepath.Join(r.checkout, ".crw", ".gitignore")); err != nil || string(ignore) != crwdir.GitignoreText {
		t.Errorf(".gitignore %q: %v", ignore, err)
	}
	if _, second := r.prompt(t, r.checkout, "019fcd00-0000-7000-8000-000000000003", "rename the worktree please"); second != "" {
		t.Errorf("the same session is told twice: %q", second)
	}
	if _, other := r.prompt(t, r.checkout, "another-session", "rename the worktree please"); other != first {
		t.Errorf("a new session gets other guidance: %q", other)
	}
	t.Run("an unrelated prompt or an unmanaged cwd is silent", func(t *testing.T) {
		if _, ctx := r.prompt(t, r.checkout, "s-quiet", "테스트 계속 돌려줘"); ctx != "" {
			t.Errorf("unrelated prompt: %q", ctx)
		}
		if _, ctx := r.prompt(t, "/tmp", "s-quiet", "rename the worktree"); ctx != "" {
			t.Errorf("unmanaged cwd: %q", ctx)
		}
		wtGone(t, filepath.Join(r.checkout, ".crw", "worktree-guard", "s-quiet.json"))
	})
	t.Run("without a session id it guides every time and writes no marker", func(t *testing.T) {
		r := newWtRig(t)
		for range 2 {
			if _, ctx := r.prompt(t, r.checkout, "", korean); ctx == "" {
				t.Fatal("no guidance")
			}
		}
		wtGone(t, filepath.Join(r.checkout, ".crw"))
	})
	t.Run("session ids that sanitize alike share one marker (oracle defect, kept)", func(t *testing.T) {
		r := newWtRig(t)
		if _, ctx := r.prompt(t, r.checkout, "a/b", korean); ctx == "" {
			t.Fatal("no guidance")
		}
		if _, ctx := r.prompt(t, r.checkout, "a-b", korean); ctx != "" {
			t.Fatalf("second session got %q", ctx)
		}
	})
	t.Run("a managed cwd that does not exist is guided but gets no marker", func(t *testing.T) {
		r := newWtRig(t)
		ghost := filepath.Join(r.slotRoot, "ghost", "repo")
		for range 2 {
			if _, ctx := r.prompt(t, ghost, "s-ghost", korean); ctx == "" {
				t.Fatal("no guidance")
			}
		}
		wtGone(t, filepath.Join(r.slotRoot, "ghost"))
	})
}

func TestHandleWorktreeGuardIgnoresWhatItCannotRead(t *testing.T) {
	r := newWtRig(t)
	good := wtPayload(t, map[string]any{"hook_event_name": "SessionStart", "cwd": r.checkout})
	deep := strings.TrimSuffix(good, "}") + ",\"x\":" + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "}"
	for name, raw := range map[string]string{
		"not json":              "not json",
		"empty":                 "",
		"an array":              "[]",
		"null":                  "null",
		"a number":              "42",
		"a BOM before the JSON": "\ufeff" + good,
		"text after the JSON":   good + " x",
		"no cwd":                `{"hook_event_name":"SessionStart"}`,
		"an empty cwd":          `{"hook_event_name":"SessionStart","cwd":""}`,
		"a numeric cwd":         `{"hook_event_name":"SessionStart","cwd":5}`,
		"a blank cwd":           `{"hook_event_name":"SessionStart","cwd":"   "}`,
		"an unknown event":      wtPayload(t, map[string]any{"hook_event_name": "Stop", "cwd": r.checkout}),
		"no event":              wtPayload(t, map[string]any{"cwd": r.checkout}),
		"nesting past 10,000 levels (platform difference, kept)": deep,
		"a non-string prompt": wtPayload(t, map[string]any{"hook_event_name": "UserPromptSubmit", "cwd": r.checkout, "session_id": "s", "prompt": 7}),
	} {
		if _, ctx := HandleWorktreeGuard(raw, r.env()); ctx != "" {
			t.Errorf("%s: answered %q", name, ctx)
		}
	}
	if parseRaw(good) == nil || parseRaw("[]") != nil {
		t.Error("parseRaw must accept one object and nothing else")
	}
}

// The marker is a file under the workspace state; a link there must neither lead the hook out of the workspace
// nor let it overwrite a record (the oracle's writeFileSync follows and truncates; port: fixed).
func TestMarkerStaysInsideTheWorkspace(t *testing.T) {
	const korean = "워크트리 이름 바꾸고 시작하자"
	const session = "link-session"
	guided := func(t *testing.T, r wtRig) {
		t.Helper()
		for range 2 {
			if _, ctx := r.prompt(t, r.checkout, session, korean); !strings.Contains(ctx, "WORKTREE-GUARD-02") {
				t.Fatalf("the guidance is lost: %q", ctx)
			}
		}
	}
	empty := func(t *testing.T, dir string) {
		t.Helper()
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("%s was written through a link: %v %v", dir, entries, err)
		}
	}
	t.Run(".crw linked out of the workspace", func(t *testing.T) {
		r, outside := newWtRig(t), t.TempDir()
		wtLink(t, outside, filepath.Join(r.checkout, ".crw"))
		guided(t, r)
		empty(t, outside)
	})
	t.Run("a dangling .crw", func(t *testing.T) {
		r, outside := newWtRig(t), t.TempDir()
		wtLink(t, filepath.Join(outside, "missing"), filepath.Join(r.checkout, ".crw"))
		guided(t, r)
		empty(t, outside)
	})
	t.Run(".crw is a regular file", func(t *testing.T) {
		r := newWtRig(t)
		wtWrite(t, filepath.Join(r.checkout, ".crw"), "plain")
		guided(t, r)
		if data, _ := os.ReadFile(filepath.Join(r.checkout, ".crw")); string(data) != "plain" {
			t.Fatalf("file replaced: %q", data)
		}
	})
	t.Run("worktree-guard linked out of the workspace", func(t *testing.T) {
		r, outside := newWtRig(t), t.TempDir()
		wtLink(t, outside, filepath.Join(r.checkout, ".crw", "worktree-guard"))
		guided(t, r)
		empty(t, outside)
	})
	t.Run("a dangling marker link", func(t *testing.T) {
		r, outside := newWtRig(t), t.TempDir()
		wtLink(t, filepath.Join(outside, "new.json"), filepath.Join(r.checkout, ".crw", "worktree-guard", session+".json"))
		guided(t, r)
		empty(t, outside)
	})
	t.Run("a marker link to a record outside leaves the record whole", func(t *testing.T) {
		r, outside := newWtRig(t), t.TempDir()
		record := filepath.Join(outside, "state.json")
		wtWrite(t, record, "{\"phase\":\"B\"}\n")
		wtLink(t, record, filepath.Join(r.checkout, ".crw", "worktree-guard", session+".json"))
		guided(t, r)
		if data, _ := os.ReadFile(record); string(data) != "{\"phase\":\"B\"}\n" {
			t.Fatalf("record rewritten: %q", data)
		}
	})
	t.Run("a marker link to a file inside the workspace counts as injected and stays whole", func(t *testing.T) {
		r := newWtRig(t)
		inside := filepath.Join(r.checkout, "keep.json")
		wtWrite(t, inside, "keep")
		wtLink(t, "../../keep.json", filepath.Join(r.checkout, ".crw", "worktree-guard", session+".json"))
		if _, ctx := r.prompt(t, r.checkout, session, korean); ctx != "" {
			t.Fatalf("answered %q", ctx)
		}
		if data, _ := os.ReadFile(inside); string(data) != "keep" {
			t.Fatalf("file rewritten: %q", data)
		}
	})
	t.Run(".crw linked inside the workspace keeps working", func(t *testing.T) {
		r := newWtRig(t)
		wtLink(t, "state", filepath.Join(r.checkout, ".crw"))
		if err := os.MkdirAll(filepath.Join(r.checkout, "state"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, ctx := r.prompt(t, r.checkout, session, korean); ctx == "" {
			t.Fatal("no guidance")
		}
		if data, err := os.ReadFile(filepath.Join(r.checkout, "state", "worktree-guard", session+".json")); err != nil || !markerJSON.Match(data) {
			t.Fatalf("marker %q: %v", data, err)
		}
	})
}

// A lone surrogate escape in cwd reads as U+FFFD, where JavaScript keeps it and prints \ud800 (platform difference, kept).
func TestSessionStartContextOfALoneSurrogateCwd(t *testing.T) {
	r := newWtRig(t)
	raw := `{"hook_event_name":"SessionStart","cwd":"` + r.checkout + `/\ud800"}`
	if _, ctx := HandleWorktreeGuard(raw, r.env()); !strings.Contains(ctx, "(cwd: "+r.checkout+"/\ufffd; slot: ") {
		t.Fatalf("got %q", ctx)
	}
}

// The marker name is the session id through state.SanitizeKey, so no id can leave the guard directory.
func TestMarkerNameIsTheSanitizedSessionId(t *testing.T) {
	r := newWtRig(t)
	guard := filepath.Join(r.checkout, ".crw", "worktree-guard")
	for session, name := range map[string]string{"../../escape": "..-..-escape.json", "///": "missing.json", "a b\tc": "a-b-c.json"} {
		if _, ctx := r.prompt(t, r.checkout, session, "워크트리 이름 바꾸고 시작하자"); ctx == "" {
			t.Fatalf("%q: no guidance", session)
		}
		if _, err := os.Stat(filepath.Join(guard, name)); err != nil {
			t.Errorf("%q: %v", session, err)
		}
	}
	if entries, err := os.ReadDir(guard); err != nil || len(entries) != 3 {
		t.Errorf("guard directory holds %v (%v)", entries, err)
	}
	wtGone(t, filepath.Join(r.slotRoot, "escape.json"))
}

// The marker goes where detection found the directory: a cwd that is a link to the checkout gets it in the checkout.
func TestMarkerIsWrittenInTheCanonicalCwd(t *testing.T) {
	r := newWtRig(t)
	link := filepath.Join(r.home, "link-checkout")
	wtLink(t, r.checkout, link)
	if _, ctx := r.prompt(t, link, "s-link", "워크트리 이름 바꾸고 시작하자"); ctx == "" {
		t.Fatal("no guidance")
	}
	if data, err := os.ReadFile(filepath.Join(r.checkout, ".crw", "worktree-guard", "s-link.json")); err != nil || !markerJSON.Match(data) {
		t.Fatalf("marker %q: %v", data, err)
	}
	wtGone(t, filepath.Join(r.home, ".crw"))
}

// The walk-up's root-slice defect can map a ghost cwd onto a real directory; detection still says managed (as the
// oracle's does), but no marker is written there, where the oracle's mkdir on the raw path fails.
func TestMappedGhostCwdGetsNoMarker(t *testing.T) {
	r := newWtRig(t)
	ghost := "/x" + strings.TrimPrefix(r.checkout, "/")
	if _, err := os.Lstat("/" + strings.SplitN(ghost, "/", 3)[1]); err == nil {
		t.Skip("the mapped top-level directory exists")
	}
	if got := detectManagedWorktree(ghost, r.env()); !got.Managed || got.CheckoutRoot != r.checkout || got.cwd != "" {
		t.Fatalf("got %+v", got)
	}
	if _, ctx := r.prompt(t, ghost, "s-mapped", "워크트리 이름 바꾸고 시작하자"); ctx == "" {
		t.Fatal("no guidance")
	}
	wtGone(t, filepath.Join(r.checkout, ".crw"))
}

// ensureStateDir is crwdir.EnsureDir through a Root: only its creator publishes the .gitignore.
func TestEnsureStateDir(t *testing.T) {
	open := func(t *testing.T) (*os.Root, string) {
		t.Helper()
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { root.Close() })
		return root, dir
	}
	t.Run("a missing directory is made with its .gitignore", func(t *testing.T) {
		root, dir := open(t)
		if err := ensureStateDir(root); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(dir, ".crw", ".gitignore")); err != nil || string(data) != crwdir.GitignoreText {
			t.Fatalf(".gitignore %q: %v", data, err)
		}
	})
	t.Run("an existing directory stays as it is, without a .gitignore", func(t *testing.T) {
		root, dir := open(t)
		if err := os.Mkdir(filepath.Join(dir, ".crw"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ensureStateDir(root); err != nil {
			t.Fatal(err)
		}
		wtGone(t, filepath.Join(dir, ".crw", ".gitignore"))
	})
	t.Run("an existing file is left alone", func(t *testing.T) {
		root, dir := open(t)
		wtWrite(t, filepath.Join(dir, ".crw"), "plain")
		if err := ensureStateDir(root); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(filepath.Join(dir, ".crw")); string(data) != "plain" {
			t.Fatalf("file replaced: %q", data)
		}
	})
}
