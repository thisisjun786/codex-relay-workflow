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
// asks: the merged pull requests inside the merged:>=T or merged:A..B window, newest first,
// at most --limit of them. It records every search it was given.
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
		sort.SliceStable(inside, func(i, j int) bool { return inside[i].MergedAt > inside[j].MergedAt })
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
	if len(w.searches) != 2 || w.searches[0] != "merged:>=2026-09-30T00:00:00Z" ||
		!strings.HasPrefix(w.searches[1], "merged:2026-09-30T00:00:00Z..") {
		t.Errorf("the searches were %v, want the open window and then one narrowed to the oldest merge time read", w.searches)
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
