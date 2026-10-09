package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// crw838Window is a fake gh that answers `pr list` like GitHub does for the search the mode
// asks: the merged pull requests inside the merged:>=T or merged:A..B window, at most --limit
// of them. gh orders a search by creation, not by merge time, so the fake answers newest created
// (highest number) first: a pull request created early and merged late sits at the end of the
// answer and is cut by --limit. It records every search it was given.
type crw838Window struct {
	entries  []auditPRListEntry
	searches []string
}

func (w *crw838Window) install(t *testing.T) {
	t.Helper()
	previous := auditPRGh
	auditPRGh = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) < 2 || args[0] != "pr" || args[1] != "list" {
			return nil, errors.New("unexpected gh arguments: " + strings.Join(args, " "))
		}
		search, limit := "", 0
		for i, arg := range args {
			switch arg {
			case "--search":
				search = args[i+1]
			case "--limit":
				if _, err := fmt.Sscanf(args[i+1], "%d", &limit); err != nil {
					return nil, err
				}
			}
		}
		w.searches = append(w.searches, search)
		window := strings.TrimPrefix(search, "merged:")
		var from, to time.Time
		if rest, ok := strings.CutPrefix(window, ">="); ok {
			from, _ = time.Parse(time.RFC3339, rest)
		} else {
			lo, hi, _ := strings.Cut(window, "..")
			from, _ = time.Parse(time.RFC3339, lo)
			to, _ = time.Parse(time.RFC3339, hi)
		}
		var inside []auditPRListEntry
		for _, entry := range w.entries {
			at, _ := time.Parse(time.RFC3339, entry.MergedAt)
			if at.Before(from) || (!to.IsZero() && at.After(to)) {
				continue
			}
			inside = append(inside, entry)
		}
		sort.SliceStable(inside, func(i, j int) bool { return inside[i].Number > inside[j].Number })
		if len(inside) > limit {
			inside = inside[:limit]
		}
		return json.Marshal(inside)
	}
	t.Cleanup(func() { auditPRGh = previous })
}

// CRW-838 (200-entry window): when the newest 200 merged pull requests are all in the ledger,
// the run reads on to older windows until it has the targets --max asks for, newest first.
func TestCRW838TheWindowIsReadOnPastTwoHundredAuditedPullRequests(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-09-30T00:00:00Z"})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := &crw838Window{}
	var ledger strings.Builder
	for n := 1; n <= 250; n++ {
		at := base.Add(time.Duration(n) * time.Hour).Format(time.RFC3339)
		w.entries = append(w.entries, auditPRListEntryOf(n, fmt.Sprintf("CRW-%d: change %d", n, n), at, fmt.Sprintf("m%d", n)))
		if n > 50 {
			fmt.Fprintf(&ledger, "{\"mode\":\"pr\",\"subject\":\"pr-%d\",\"status\":\"ok\"}\n", n)
		}
	}
	w.install(t)
	if err := os.MkdirAll(filepath.Join(state, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "audit", auditLedgerFile), []byte(ledger.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	assignments := map[string]string{}
	settings := map[string]string{}
	for _, n := range []int{50, 49, 48} {
		assignments[fmt.Sprintf("CRW-%d", n)] = auditPRAssignmentJSON(t, fmt.Sprintf("rel-%d", n), fmt.Sprintf("child-%d", n))
		settings[fmt.Sprintf("child-%d", n)] = auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")
	}
	auditPRFakeRelay(t, assignments, settings, nil)
	e, out, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 2, true); code != 0 {
		t.Fatalf("audit pr --dry-run --max 2: exit %d %q", code, errOut.String())
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var entry auditPRDryRun
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		subjects = append(subjects, entry.Subject)
	}
	if strings.Join(subjects, ",") != "pr-50,pr-49" {
		t.Errorf("the targets are %v, want pr-50 and pr-49 (newest unaudited first, two of them)", subjects)
	}
	// The first search is the open window; the full answer is split by merge time and read on.
	if len(w.searches) < 2 || w.searches[0] != "merged:>=2026-09-30T00:00:00Z" {
		t.Errorf("the searches were %v, want the open window and then the split ranges", w.searches)
	}
}

// A window that is not full is the whole answer: no second search is made.
func TestCRW838AShortWindowIsNotReadOnPast(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-09-30T00:00:00Z"})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := &crw838Window{}
	for n := 1; n <= 5; n++ {
		w.entries = append(w.entries, auditPRListEntryOf(n, fmt.Sprintf("CRW-%d: change", n), base.Add(time.Duration(n)*time.Hour).Format(time.RFC3339), fmt.Sprintf("m%d", n)))
	}
	w.install(t)
	auditPRFakeRelay(t, map[string]string{}, map[string]string{}, nil)
	e, _, _ := auditTestEnv(t)
	auditPRRunWith(context.Background(), e, cfg, 9, true)
	if len(w.searches) != 1 {
		t.Errorf("a short window was followed by %v", w.searches)
	}
}

// CRW-838 (fix round 2): gh orders the search by creation, not by merge time. A pull request
// created first and merged last falls outside the first full answer, and the 200 that answer
// holds are all audited: the run still reaches it, through auditPRRunWith, with nothing lost.
func TestCRW838ACreationOrderedSearchDoesNotLoseALateMerge(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-09-30T00:00:00Z"})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := &crw838Window{}
	var ledger strings.Builder
	for n := 1; n <= 201; n++ {
		at := base.Add(time.Duration(n) * time.Hour)
		if n == 1 {
			// Created first, merged after every other one.
			at = base.Add(500 * time.Hour)
		}
		w.entries = append(w.entries, auditPRListEntryOf(n, fmt.Sprintf("CRW-%d: change %d", n, n), at.Format(time.RFC3339), fmt.Sprintf("m%d", n)))
		if n > 1 {
			fmt.Fprintf(&ledger, "{\"mode\":\"pr\",\"subject\":\"pr-%d\",\"status\":\"ok\"}\n", n)
		}
	}
	w.install(t)
	if err := os.MkdirAll(filepath.Join(state, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "audit", auditLedgerFile), []byte(ledger.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	auditPRFakeRelay(t, map[string]string{"CRW-1": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")}, nil)
	e, out, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 1, true); code != 0 {
		t.Fatalf("audit pr --dry-run --max 1: exit %d %q (searches %v)", code, errOut.String(), w.searches)
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var entry auditPRDryRun
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		subjects = append(subjects, entry.Subject)
	}
	if strings.Join(subjects, ",") != "pr-1" {
		t.Fatalf("the targets are %v, want pr-1 (merged last, created first, outside the first full answer); searches %v", subjects, w.searches)
	}
}

// Whatever order gh answers in, the windows read before the run stops cover every merge time from
// the newest down: a newer unaudited pull request is never passed over for an older one.
func TestCRW838TheTargetsAreNewestMergedFirstWhateverTheSearchOrder(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-09-30T00:00:00Z"})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	w := &crw838Window{}
	assignments := map[string]string{}
	settings := map[string]string{}
	for n := 1; n <= 450; n++ {
		// The merge order is the reverse of the creation order.
		at := base.Add(time.Duration(1000-n) * time.Hour).Format(time.RFC3339)
		w.entries = append(w.entries, auditPRListEntryOf(n, fmt.Sprintf("CRW-%d: change %d", n, n), at, fmt.Sprintf("m%d", n)))
		assignments[fmt.Sprintf("CRW-%d", n)] = auditPRAssignmentJSON(t, fmt.Sprintf("rel-%d", n), fmt.Sprintf("child-%d", n))
		settings[fmt.Sprintf("child-%d", n)] = auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")
	}
	w.install(t)
	auditPRFakeRelay(t, assignments, settings, nil)
	e, out, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 3, true); code != 0 {
		t.Fatalf("audit pr --dry-run --max 3: exit %d %q", code, errOut.String())
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var entry auditPRDryRun
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		subjects = append(subjects, entry.Subject)
	}
	if strings.Join(subjects, ",") != "pr-1,pr-2,pr-3" {
		t.Errorf("the targets are %v, want pr-1, pr-2, pr-3 (merged last); searches %v", subjects, w.searches)
	}
}

// A merge time gh answered that does not parse cannot place its range, so the run is refused by
// name instead of reading on as if the list were whole, whether or not the title carries a key.
func TestCRW838AnUnparsableMergeTimeRefusesTheRun(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-09-30T00:00:00Z"})
	previous := auditPRGh
	auditPRGh = func(_ context.Context, _ ...string) ([]byte, error) {
		return json.Marshal([]auditPRListEntry{auditPRListEntryOf(7, "no key here", "yesterday", "m7")})
	}
	t.Cleanup(func() { auditPRGh = previous })
	auditPRFakeRelay(t, map[string]string{}, map[string]string{}, nil)
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 1, true); code != 1 || !strings.Contains(errOut.String(), "the merged time of pull request #7") {
		t.Fatalf("exit %d %q, want 1 and the named entry", code, errOut.String())
	}
}
