package managed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// titleFake is the managed tests' scripted fake host with a record of every title the engine
// handed it: the name thread/start carried and the name thread/name/set carried.
type titleFake struct {
	*scriptedApp
	startTitles []string
	renameNames []string
}

func (h *titleFake) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	h.startTitles = append(h.startTitles, in.Title)
	return h.scriptedApp.CreateThread(ctx, in)
}

func (h *titleFake) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "thread/name/set" {
		h.renameNames = append(h.renameNames, pyjson.Text(params["name"]))
	}
	return h.scriptedApp.HostCall(ctx, method, params)
}

// titleKit is one managed start over a scripted App Server, with the request's issue key and
// child title chosen by the test.
type titleKit struct {
	t        *testing.T
	start    *Start
	host     *titleFake
	clock    *stepClock
	store    *store.Store
	raw      []byte
	request  map[string]any
	issueKey string
	rawTitle string
}

func titleRequest(t *testing.T, issueKey, title string) []byte {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(requestFixture(t), &request); err != nil {
		t.Fatal(err)
	}
	request["issueKey"] = issueKey
	request["criteriaSource"] = "issue:" + issueKey
	request["scopeRef"] = "issue:" + issueKey
	pyjson.Map(request["child"])["title"] = title
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newTitleKit(t *testing.T, issueKey, title string, outcomes ...string) *titleKit {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw := titleRequest(t, issueKey, title)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	settings := pyjson.Map(pyjson.Map(req["child"])["settings"])
	clock := &stepClock{now: time.Date(2026, 10, 3, 8, 22, 0, 0, time.UTC)}
	fake := &managedFake{operations: map[string]map[string]any{}, settings: settings, ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	app := &scriptedApp{managedFake: fake, clock: clock, cwd: pyjson.Text(settings["cwd"]), outcomes: outcomes, threads: map[string]*appThread{}, failures: map[string]error{}, noTurn: map[string]string{}}
	host := &titleFake{scriptedApp: app}
	start := &Start{Store: s, Adapter: host, Now: clock.ISO, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	return &titleKit{t: t, start: start, host: host, clock: clock, store: s, raw: raw, request: req, issueKey: issueKey, rawTitle: title}
}

func (k *titleKit) run() map[string]any {
	k.t.Helper()
	result, err := k.start.Run(context.Background(), k.raw)
	if err != nil {
		k.t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	return got
}

// The rule the issue fixes: an empty title sends no name, a title already carrying the key is
// sent as it is, and every other title takes the key and a middle dot.
func TestChildTitleRule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, key, title, want string }{
		{"empty title sends no name", "CRW-697", "", ""},
		{"bare title takes the key", "CRW-697", "메모리 표본 기록", "CRW-697 · 메모리 표본 기록"},
		{"middle dot form is already named", "CRW-697", "CRW-697 · x", "CRW-697 · x"},
		{"colon form is already named", "CRW-697", "CRW-697: x", "CRW-697: x"},
		{"space form is already named", "CRW-697", "CRW-697 x", "CRW-697 x"},
		{"the key alone is already named", "CRW-697", "CRW-697", "CRW-697"},
		{"a hyphen after the key still separates", "CRW-697", "CRW-697-x", "CRW-697-x"},
		{"a longer key is not this key", "CRW-697", "CRW-69 · x", "CRW-697 · CRW-69 · x"},
		{"a digit after the key is part of the key", "CRW-697", "CRW-6977 x", "CRW-697 · CRW-6977 x"},
		{"a letter after the key is part of the key", "CRW-697", "CRW-697한 x", "CRW-697 · CRW-697한 x"},
		{"the key is matched as written", "CRW-697", "crw-697 x", "CRW-697 · crw-697 x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := childTitle(c.key, c.title); got != c.want {
				t.Fatalf("childTitle(%q, %q) = %q, want %q", c.key, c.title, got, c.want)
			}
		})
	}
}

// A managed start whose child title lacks the key hands the host the prefixed name on thread/start.
func TestChildTitleManagedStartPrefixesTheThreadName(t *testing.T) {
	t.Parallel()
	k := newTitleKit(t, "CRW-697", "메모리 표본 기록", "accept")
	got := k.run()
	if got["state"] != "admitted" {
		t.Fatalf("state %v", got)
	}
	want := "CRW-697 · 메모리 표본 기록"
	if len(k.host.startTitles) != 1 || k.host.startTitles[0] != want {
		t.Fatalf("thread/start titles %q, want [%q]", k.host.startTitles, want)
	}
	if len(k.host.renameNames) != 0 {
		t.Fatalf("a creation that answered accepted renames nothing: %q", k.host.renameNames)
	}

	// The stored request is the parent's: its title and its fingerprint are unchanged, so the
	// same request repeats as a replay.
	if pyjson.Text(pyjson.Map(k.request["child"])["title"]) != k.rawTitle {
		t.Fatalf("the request's child title was rewritten: %q", pyjson.Map(k.request["child"])["title"])
	}
	identity, err := RequestIdentity(context.Background(), k.request, k.store, k.start.Socket, k.start.MarkerRoot, k.start.StateSelector, k.host.ledger)
	if err != nil {
		t.Fatal(err)
	}
	row, err := k.store.ManagedStartRequest(context.Background(), "managed-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.RequestFingerprint != identity.Fingerprint {
		t.Fatalf("stored fingerprint %q, want %q", row.RequestFingerprint, identity.Fingerprint)
	}

	replay := k.run()
	if replay["state"] != "admitted" || k.host.created != 1 || k.host.sent != 1 || len(k.host.startTitles) != 1 || len(k.host.renameNames) != 0 {
		t.Fatalf("the repeat is a replay: %v created %d sent %d titles %q renames %q", replay, k.host.created, k.host.sent, k.host.startTitles, k.host.renameNames)
	}
}

// The rename after an adopted standby carries the same prefixed name, and a replay names nothing again.
func TestChildTitleAdoptedStandbyRenamesWithThePrefixedName(t *testing.T) {
	t.Parallel()
	k := newTitleKit(t, "CRW-697", "메모리 표본 기록", "lost-applied")
	got := k.run()
	if got["state"] != "admitted" {
		t.Fatalf("state %v", got)
	}
	want := "CRW-697 · 메모리 표본 기록"
	if len(k.host.startTitles) != 1 || k.host.startTitles[0] != want {
		t.Fatalf("thread/start titles %q, want [%q]", k.host.startTitles, want)
	}
	if len(k.host.renameNames) != 1 || k.host.renameNames[0] != want {
		t.Fatalf("thread/name/set names %q, want [%q]", k.host.renameNames, want)
	}
	if got["childTaskId"] != "t-1" {
		t.Fatalf("child %v", got["childTaskId"])
	}
	replay := k.run()
	// One run sends the standby recovery and the business turn; the replay sends nothing and names nothing.
	if replay["state"] != "admitted" || k.host.created != 1 || k.host.sent != 2 || len(k.host.renameNames) != 1 {
		t.Fatalf("the repeat is a replay: %v created %d sent %d renames %q", replay, k.host.created, k.host.sent, k.host.renameNames)
	}
}

// A title that already carries the key is sent exactly as the parent wrote it.
func TestChildTitleAlreadyPrefixedIsSentUnchanged(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"CRW-697 · 메모리 표본 기록", "CRW-697: 메모리 표본 기록", "CRW-697 메모리 표본 기록"} {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			k := newTitleKit(t, "CRW-697", title, "accept")
			k.run()
			if len(k.host.startTitles) != 1 || k.host.startTitles[0] != title {
				t.Fatalf("thread/start titles %q, want [%q]", k.host.startTitles, title)
			}
		})
	}
}

// A key that is only a prefix of the title's own key is not the key: the title takes the prefix.
func TestChildTitleShorterKeyIsNotTheKey(t *testing.T) {
	t.Parallel()
	k := newTitleKit(t, "CRW-697", "CRW-69 · x", "accept")
	k.run()
	want := "CRW-697 · CRW-69 · x"
	if len(k.host.startTitles) != 1 || k.host.startTitles[0] != want {
		t.Fatalf("thread/start titles %q, want [%q]", k.host.startTitles, want)
	}
}

// A thread a lost creation left, whose host name already carries the prefix, is the same creation:
// the engine adopts it instead of creating a second thread after the grace period.
func TestChildTitleLostCreationIsAdoptedByItsPrefixedName(t *testing.T) {
	t.Parallel()
	k := newTitleKit(t, "CRW-697", "메모리 표본 기록", "lost")
	got := k.run()
	if got["state"] != "incomplete" || pyjson.Text(got["reason"]) != "creation_unknown" {
		t.Fatalf("first answer %v", got)
	}
	if k.host.created != 1 || len(k.host.startTitles) != 1 {
		t.Fatalf("one creation so far: %d %q", k.host.created, k.host.startTitles)
	}
	// The creation's thread/start reached the host and set the prefixed name before its answer was lost.
	k.host.addThread()
	k.host.threads["t-1"].name = "CRW-697 · 메모리 표본 기록"
	k.clock.now = k.clock.now.Add(3 * time.Minute)
	again := k.run()
	if again["state"] != "admitted" || again["childTaskId"] != "t-1" {
		t.Fatalf("the thread the creation left is adopted: %v", again)
	}
	if k.host.created != 1 {
		t.Fatalf("a duplicate thread was created: %d creations %q", k.host.created, k.host.startTitles)
	}
}
