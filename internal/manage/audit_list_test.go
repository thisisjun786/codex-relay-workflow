package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// auditListHome points HOME, CODEX_HOME, CRW_HOME and XDG_STATE_HOME at a fresh temporary
// tree and returns the manage state directory below it, so no test reaches the real ones.
func auditListHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	t.Setenv("XDG_STATE_HOME", state)
	return filepath.Join(state, "crw", "manage")
}

// auditListScore is a score pointer, as a ledger or alert row carries it.
func auditListScore(value int) *int { return &value }

// auditListLedgerRow is one graded result as a fixture names it.
func auditListLedgerRow(issue, round, gradedAt string) auditLedgerRow {
	return auditLedgerRow{
		Mode: auditModePR, Subject: "subject-" + issue, Head: "head-" + issue, Issue: issue,
		Pair: "pair", Phase: "live", Round: round, Status: auditStatusOK, Score: auditListScore(7),
		P0: 0, P1: 1, P2: 0, P3: 0, GradedAt: gradedAt, Bundle: "bundle-" + issue,
	}
}

// auditListAppendJSONL appends one JSON document per line to a file below a state directory,
// creating the directory and the file, so a fixture can put a deliberately broken line in
// the middle of a real one.
func auditListAppendJSONL(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// auditListJSONLine is one value as a JSONL line.
func auditListJSONLine(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditListWriteRound writes one round file, with the given packages, below a state
// directory.
func auditListWriteRound(t *testing.T, state, name, startedAt string, packages ...auditRoundPackage) {
	t.Helper()
	dir := filepath.Join(state, "audit", "rounds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(auditRoundFile{Round: name, StartedAt: startedAt, Packages: packages}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// auditListWriteDraft writes one draft file below a state directory.
func auditListWriteDraft(t *testing.T, state, fingerprint, project, severity, draftState string) {
	t.Helper()
	dir := filepath.Join(state, "drafts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := auditDraft{
		Schema: auditDraftSchema, Fingerprint: fingerprint, Source: auditDraftSource,
		Project: project, Title: "title-" + fingerprint, Severity: severity, Body: "body",
		Labels: auditDraftLabels(severity), State: draftState,
		Seen: []auditDraftSeen{{Mode: auditModePR, Subject: "s", Head: "h", At: "2026-01-01T00:00:00Z"}},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fingerprint+".json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// auditListFixture fills a state directory with one of each source and returns the
// configuration the command and the function both read it with.
func auditListFixture(t *testing.T, e *Env) (string, *Config) {
	t.Helper()
	state := auditListHome(t)
	auditListAppendJSONL(t, filepath.Join(state, "audit"), auditLedgerFile,
		auditListJSONLine(t, auditListLedgerRow("CRW-1", "r1", "2026-01-01T00:00:00Z")),
		auditListJSONLine(t, auditListLedgerRow("CRW-2", "r2", "2026-01-01T00:00:00.500Z")))
	auditListAppendJSONL(t, filepath.Join(state, "audit"), auditAlertFile,
		auditListJSONLine(t, auditAlertRow{
			Mode: auditModePR, Subject: "subject-CRW-1", Issue: "CRW-1", Pair: "pair", Phase: "live",
			Round: "r1", Score: auditListScore(4),
			Defects: []auditAlertDefect{{Severity: "P1", What: "wrong", Where: "a.go:1"}},
		}))
	auditListWriteRound(t, state, "r1", "2026-01-01T00:00:00Z",
		auditRoundPackage{Package: "pkg/a", State: auditRoundAudited, Status: auditStatusOK, P1: 1},
		auditRoundPackage{Package: "pkg/b", State: auditRoundPending})
	auditListWriteDraft(t, state, "bbbbbbbbbbbbbbbb", "CRW", "P1", auditDraftStateDraft)
	auditListWriteDraft(t, state, "aaaaaaaaaaaaaaaa", "CRW", "P2", auditDraftStatePosted)
	return state, coreDefaults(e)
}

// auditListSnapshot is every entry below a root as name, size and modification time, sorted,
// so two snapshots compare a directory tree exactly.
func auditListSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s|%d|%d", rel, info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// auditListSourceOf is one source of a listing by name.
func auditListSourceOf(t *testing.T, listing AuditListing, name string) auditListSource {
	t.Helper()
	for _, source := range listing.Sources {
		if source.Name == name {
			return source
		}
	}
	t.Fatalf("the listing names no source %q: %+v", name, listing.Sources)
	return auditListSource{}
}

// auditListCommandDocument runs the command and decodes the one document it prints.
func auditListCommandDocument(t *testing.T, e *Env, args ...string) (AuditListing, int, string) {
	t.Helper()
	e.Stdout.(*strings.Builder).Reset()
	e.Stderr.(*strings.Builder).Reset()
	code := auditRun(context.Background(), e, append([]string{"list"}, args...))
	out, _ := e.Stdout.(*strings.Builder)
	var listing AuditListing
	if out.Len() > 0 {
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &listing); err != nil {
			t.Fatalf("the command printed %q, which is not one document: %v", out.String(), err)
		}
	}
	return listing, code, out.String()
}

// C1: the ledger, the alert queue, the round files and the draft files are read into one
// crw-audit-list/1 document, and the command prints exactly the document AuditList returns.
func TestAuditListReadsEveryRecord(t *testing.T) {
	e, out, _ := auditTestEnv(t)
	state, cfg := auditListFixture(t, e)

	listing, err := AuditList(context.Background(), e, cfg, AuditListOptions{})
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	if listing.Schema != auditListSchema || listing.StateDir != state {
		t.Errorf("the document names schema %q and state_dir %q", listing.Schema, listing.StateDir)
	}
	if want := e.Now().UTC().Format(time.RFC3339); listing.ReadAt != want {
		t.Errorf("read_at = %q, want %q", listing.ReadAt, want)
	}
	if names := []string{"ledger", "alerts", "rounds", "drafts"}; len(listing.Sources) != len(names) {
		t.Fatalf("the listing names %d sources, want %d", len(listing.Sources), len(names))
	} else {
		for i, name := range names {
			if listing.Sources[i].Name != name {
				t.Errorf("source %d is %q, want %q", i, listing.Sources[i].Name, name)
			}
			if listing.Sources[i].State != auditListStateOK {
				t.Errorf("source %q is %q, want %q", name, listing.Sources[i].State, auditListStateOK)
			}
		}
	}
	if len(listing.Results) != 2 || listing.Results[0].Issue != "CRW-1" || listing.Results[1].Round != "r2" {
		t.Errorf("the results are %+v, want the two ledger rows in file order", listing.Results)
	}
	if len(listing.Alerts) != 1 || len(listing.Alerts[0].Defects) != 1 || listing.Alerts[0].Defects[0].Where != "a.go:1" {
		t.Errorf("the alerts are %+v, want the one alert row with its defect", listing.Alerts)
	}
	if len(listing.Rounds) != 1 {
		t.Fatalf("the rounds are %+v, want one", listing.Rounds)
	}
	if got := listing.Rounds[0]; got.Round != "r1" || got.StartedAt != "2026-01-01T00:00:00Z" ||
		got.Total != 2 || got.Audited != 1 || got.Pending != 1 || got.Failed != 0 || got.P1 != 1 || got.Clean {
		t.Errorf("the round summary is %+v", got)
	}
	if len(listing.Drafts) != 2 || listing.Drafts[0].Fingerprint != "aaaaaaaaaaaaaaaa" {
		t.Errorf("the drafts are %+v, want both in fingerprint order", listing.Drafts)
	}
	if listing.Drafts[1].Severity != "P1" || listing.Drafts[1].State != auditDraftStateDraft || listing.Drafts[1].Seen != 1 {
		t.Errorf("the second draft summary is %+v", listing.Drafts[1])
	}
	want, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("AuditList wrote %q to the env's stdout", out.String())
	}
	got, code, raw := auditListCommandDocument(t, e)
	if code != 0 {
		t.Fatalf("the command exited %d, want 0 (stderr %q)", code, e.Stderr.(*strings.Builder).String())
	}
	if raw != string(want)+"\n" {
		t.Errorf("the command printed\n%s\nwant\n%s", raw, string(want))
	}
	if !reflect.DeepEqual(got, listing) {
		t.Errorf("the command's document is %+v, want %+v", got, listing)
	}
	// A present but empty source is ok with an empty list, never null: null is the unknown
	// sentinel a caller reads as "could not read this source".
	emptyEnv, _, _ := auditTestEnv(t)
	empty := auditListHome(t)
	auditListAppendJSONL(t, filepath.Join(empty, "audit"), auditLedgerFile, "")
	emptyListing, err := AuditList(context.Background(), emptyEnv, coreDefaults(emptyEnv), AuditListOptions{})
	if err != nil {
		t.Fatalf("AuditList over an empty ledger: %v", err)
	}
	if emptyListing.StateDir != empty {
		t.Fatalf("the empty listing read %q, want %q", emptyListing.StateDir, empty)
	}
	if source := auditListSourceOf(t, emptyListing, "ledger"); source.State != auditListStateOK {
		t.Errorf("an empty ledger file is %q, want %q", source.State, auditListStateOK)
	}
	emptyData, err := json.Marshal(emptyListing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(emptyData), `"results":[]`) {
		t.Errorf("an empty ledger file serialized %s, want an empty results list", emptyData)
	}
}

// C2: a call reads only, and a state directory without an audit directory stays without one.
func TestAuditListWritesNothing(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state, cfg := auditListFixture(t, e)
	before := auditListSnapshot(t, filepath.Dir(state))
	if _, err := AuditList(context.Background(), e, cfg, AuditListOptions{}); err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	if _, code, _ := auditListCommandDocument(t, e); code != 0 {
		t.Fatalf("the command exited %d, want 0", code)
	}
	after := auditListSnapshot(t, filepath.Dir(state))
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the state tree changed:\nbefore %v\nafter  %v", before, after)
	}

	empty := auditListHome(t)
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	emptyBefore := auditListSnapshot(t, filepath.Dir(empty))
	emptyEnv, _, _ := auditTestEnv(t)
	listing, err := AuditList(context.Background(), emptyEnv, coreDefaults(emptyEnv), AuditListOptions{})
	if err != nil {
		t.Fatalf("AuditList without an audit directory: %v", err)
	}
	if listing.StateDir != empty {
		t.Fatalf("the listing read %q, want %q", listing.StateDir, empty)
	}
	for _, source := range listing.Sources {
		if source.State != auditListStateAbsent {
			t.Errorf("source %q is %q without an audit directory, want %q", source.Name, source.State, auditListStateAbsent)
		}
	}
	if listing.Results == nil || listing.Alerts == nil || listing.Rounds == nil || listing.Drafts == nil {
		t.Errorf("an absent source carries a nil list: %+v", listing)
	}
	data, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"results":[]`, `"alerts":[]`, `"rounds":[]`, `"drafts":[]`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("an absent source serialized %s, want %s", data, key)
		}
	}
	if _, code, _ := auditListCommandDocument(t, emptyEnv); code != 0 {
		t.Errorf("the command exited %d without an audit directory, want 0", code)
	}
	if emptyAfter := auditListSnapshot(t, filepath.Dir(empty)); !reflect.DeepEqual(emptyBefore, emptyAfter) {
		t.Errorf("the state tree gained entries:\nbefore %v\nafter  %v", emptyBefore, emptyAfter)
	}
}

// C3: a source that cannot be read is unknown with a reason and a null list, and the other
// sources keep their rows.
func TestAuditListUnreadableSourceIsUnknown(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(t *testing.T, state string)
		want    string
		listKey string
		reason  string
	}{
		{
			name: "a broken ledger line",
			break_: func(t *testing.T, state string) {
				auditListAppendJSONL(t, filepath.Join(state, "audit"), auditLedgerFile,
					auditListJSONLine(t, auditListLedgerRow("CRW-1", "r1", "2026-01-01T00:00:00Z")),
					auditListJSONLine(t, auditListLedgerRow("CRW-2", "r2", "2026-01-01T00:00:00Z")),
					"{\"mode\":")
			},
			want: "ledger", listKey: "results", reason: "line 3",
		},
		{
			name: "a broken round file",
			break_: func(t *testing.T, state string) {
				dir := filepath.Join(state, "audit", "rounds")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "r9.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "rounds", listKey: "rounds", reason: "r9.json",
		},
		{
			name: "a broken draft file",
			break_: func(t *testing.T, state string) {
				dir := filepath.Join(state, "drafts")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "cccccccccccccccc.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "drafts", listKey: "drafts", reason: "cccccccccccccccc.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := auditTestEnv(t)
			state, cfg := auditListFixture(t, e)
			tc.break_(t, state)
			listing, err := AuditList(context.Background(), e, cfg, AuditListOptions{})
			if err != nil {
				t.Fatalf("AuditList: %v", err)
			}
			source := auditListSourceOf(t, listing, tc.want)
			if source.State != auditListStateUnknown {
				t.Errorf("source %q is %q, want %q", tc.want, source.State, auditListStateUnknown)
			}
			if !strings.Contains(source.Reason, tc.reason) {
				t.Errorf("source %q names the reason %q, want it to name %q", tc.want, source.Reason, tc.reason)
			}
			for _, other := range listing.Sources {
				if other.Name == tc.want {
					continue
				}
				if other.State != auditListStateOK {
					t.Errorf("source %q is %q, want %q", other.Name, other.State, auditListStateOK)
				}
			}
			lists := map[string][]int{
				"ledger": {len(listing.Results)}, "alerts": {len(listing.Alerts)},
				"rounds": {len(listing.Rounds)}, "drafts": {len(listing.Drafts)},
			}
			data, err := json.Marshal(listing)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"`+tc.listKey+`":null`) {
				t.Errorf("source %q did not serialize a null %s list: %s", tc.want, tc.listKey, data)
			}
			if tc.want != "ledger" && lists["ledger"][0] != 2 {
				t.Errorf("the ledger lost rows: %d", lists["ledger"][0])
			}
			if tc.want != "alerts" && lists["alerts"][0] != 1 {
				t.Errorf("the alerts lost rows: %d", lists["alerts"][0])
			}
			if tc.want != "rounds" && lists["rounds"][0] != 1 {
				t.Errorf("the rounds lost rows: %d", lists["rounds"][0])
			}
			if tc.want != "drafts" && lists["drafts"][0] != 2 {
				t.Errorf("the drafts lost rows: %d", lists["drafts"][0])
			}
			_, code, _ := auditListCommandDocument(t, e)
			if code != 1 {
				t.Errorf("the command exited %d with an unknown source, want 1", code)
			}
		})
	}
}

// C4: the three filters select what the issue fixes, comparing instants, and an empty value
// is a usage error.
func TestAuditListFilters(t *testing.T) {
	e, _, errOut := auditTestEnv(t)
	state := auditListHome(t)
	auditListAppendJSONL(t, filepath.Join(state, "audit"), auditLedgerFile,
		auditListJSONLine(t, auditListLedgerRow("CRW-1", "r1", "2026-01-01T00:00:00Z")),
		auditListJSONLine(t, auditListLedgerRow("CRW-1", "r2", "2026-01-01T00:00:00.500Z")),
		auditListJSONLine(t, auditListLedgerRow("CRW-2", "r1", "2025-12-31T15:00:00-09:00")),
		auditListJSONLine(t, auditListLedgerRow("CRW-2", "r2", "not-a-time")))
	auditListAppendJSONL(t, filepath.Join(state, "audit"), auditAlertFile,
		auditListJSONLine(t, auditAlertRow{Mode: auditModePR, Issue: "CRW-1", Round: "r1"}),
		auditListJSONLine(t, auditAlertRow{Mode: auditModePR, Issue: "CRW-2", Round: "r2"}))
	auditListWriteRound(t, state, "r1", "2026-01-01T00:00:00Z", auditRoundPackage{Package: "pkg/a", State: auditRoundPending})
	auditListWriteRound(t, state, "r2", "2026-01-02T00:00:00Z", auditRoundPackage{Package: "pkg/b", State: auditRoundPending})
	auditListWriteDraft(t, state, "aaaaaaaaaaaaaaaa", "CRW", "P1", auditDraftStateDraft)
	cfg := coreDefaults(e)

	issues := func(rows []auditLedgerRow) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.Issue+"/"+row.Round)
		}
		return out
	}
	call := func(t *testing.T, opts AuditListOptions) AuditListing {
		t.Helper()
		listing, err := AuditList(context.Background(), e, cfg, opts)
		if err != nil {
			t.Fatalf("AuditList: %v", err)
		}
		return listing
	}

	byRound := call(t, AuditListOptions{Round: "r1"})
	if got := issues(byRound.Results); !reflect.DeepEqual(got, []string{"CRW-1/r1", "CRW-2/r1"}) {
		t.Errorf("--round r1 selected %v", got)
	}
	if len(byRound.Alerts) != 1 || byRound.Alerts[0].Issue != "CRW-1" {
		t.Errorf("--round r1 selected alerts %+v", byRound.Alerts)
	}
	if len(byRound.Rounds) != 1 || byRound.Rounds[0].Round != "r1" {
		t.Errorf("--round r1 selected rounds %+v", byRound.Rounds)
	}
	if len(byRound.Drafts) != 1 {
		t.Errorf("--round left the drafts alone: %+v", byRound.Drafts)
	}

	byIssue := call(t, AuditListOptions{Issue: "CRW-2"})
	if got := issues(byIssue.Results); !reflect.DeepEqual(got, []string{"CRW-2/r1", "CRW-2/r2"}) {
		t.Errorf("--issue CRW-2 selected %v", got)
	}
	if len(byIssue.Alerts) != 1 || byIssue.Alerts[0].Issue != "CRW-2" {
		t.Errorf("--issue CRW-2 selected alerts %+v", byIssue.Alerts)
	}
	if len(byIssue.Rounds) != 2 {
		t.Errorf("--issue left the rounds alone: %+v", byIssue.Rounds)
	}
	if len(byIssue.Drafts) != 1 {
		t.Errorf("--issue left the drafts alone: %+v", byIssue.Drafts)
	}

	at := time.Date(2026, 1, 1, 0, 0, 0, 250000000, time.UTC)
	bySince := call(t, AuditListOptions{Since: at, HasSince: true})
	if got := issues(bySince.Results); !reflect.DeepEqual(got, []string{"CRW-1/r2", "CRW-2/r2"}) {
		t.Errorf("--since %s selected %v, want the later row and the undateable one", at.Format(time.RFC3339Nano), got)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	byInstant := call(t, AuditListOptions{Since: from, HasSince: true})
	if got := issues(byInstant.Results); !reflect.DeepEqual(got, []string{"CRW-1/r1", "CRW-1/r2", "CRW-2/r1", "CRW-2/r2"}) {
		t.Errorf("--since %s selected %v, want every row at or after it, including the offset row", from.Format(time.RFC3339Nano), got)
	}
	none := call(t, AuditListOptions{Round: "r9"})
	if none.Results == nil || len(none.Results) != 0 {
		t.Errorf("a filter that matches nothing gave %v, want an empty list", none.Results)
	}
	if data, err := json.Marshal(none); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), `"results":[]`) {
		t.Errorf("a filter that matches nothing serialized %s", data)
	}

	for _, args := range [][]string{
		{"--round="}, {"--round", ""}, {"--round", " "},
		{"--issue="}, {"--issue", ""}, {"--since="}, {"--since", ""},
	} {
		errOut.Reset()
		e.Stdout.(*strings.Builder).Reset()
		if code := auditRun(context.Background(), e, append([]string{"list"}, args...)); code != usageExit {
			t.Errorf("%q exited %d, want %d", args, code, usageExit)
		}
		if !strings.Contains(errOut.String(), auditListUsage) {
			t.Errorf("%q did not print the usage: %q", args, errOut.String())
		}
	}
}

// auditListFailWriter is a stdout whose every write fails, so a command that reports a
// truncated document as success fails this test.
type auditListFailWriter struct{}

func (auditListFailWriter) Write([]byte) (int, error) { return 0, errors.New("the stream is closed") }

// C5: a JSON output write that fails is exit 3.
func TestAuditListOutputWriteFailure(t *testing.T) {
	e, _, errOut := auditTestEnv(t)
	if _, cfg := auditListFixture(t, e); cfg == nil {
		t.Fatal("the fixture named no configuration")
	}
	e.Stdout = auditListFailWriter{}
	if code := auditRun(context.Background(), e, []string{"list"}); code != 3 {
		t.Errorf("a failed output write exited %d, want 3: %q", code, errOut.String())
	}
}
