package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ---------------------------------------------------------------- the review-766 cases

// relayReadReview766Store is a temporary relay store at one chosen directory: the shared
// fixture always builds below its own t.TempDir(), and these cases need two stores whose
// locations are the point of the test.
func relayReadReview766Store(t *testing.T, dir, issue string) *dagReviewFixture {
	t.Helper()
	coreTempHome(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the store directory: %v", err)
	}
	path := filepath.Join(dir, relayReadStoreFile)
	st, err := store.Open(context.Background(), path, filepath.Join(dir, "app-server-control.sock"))
	if err != nil {
		t.Fatalf("create the store: %v", err)
	}
	f := &dagReviewFixture{t: t, dir: dir, path: path, store: st}
	t.Cleanup(f.close)
	f.relayReadRelationship("rel-"+issue, issue, "active", "parent-1", "child-"+issue, 1)
	f.relayReadScope("rel-"+issue, "project-1")
	return f
}

// relayReadReview766Spelling is a state directory spelled through a symbolic link and a "..",
// built by concatenation so no Join or Clean touches it before the read does.
func relayReadReview766Spelling(root string) string { return root + "/link/../state" }

// relayReadReview766CallsWhenAny is the fake relay's recorded calls when it wrote a record, or a
// nil slice when it was never run at all: coreFakeCalls fails the test on a missing record, and
// the point of these cases is exactly that the record is absent.
func relayReadReview766CallsWhenAny(t *testing.T, record string) ([][]string, error) {
	t.Helper()
	if _, err := os.Stat(record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return coreFakeCalls(t, record), nil
}

// C1: the store this command reads is the one the filesystem means by the configured spelling.
// <tmp>/link points at <tmp>/real/inner, so <tmp>/link/../state means <tmp>/real/state; the
// real store lives there and a decoy store lives at <tmp>/state, which is where cleaning the
// spelling before the link is resolved would look.
func TestRelayReadReview766StateSpellingThroughALink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "inner"), 0o755); err != nil {
		t.Fatalf("create the link target: %v", err)
	}
	real := relayReadReview766Store(t, filepath.Join(root, "real", "state"), "CRW-REAL")
	real.close()
	decoy := relayReadReview766Store(t, filepath.Join(root, "state"), "CRW-CLEAN")
	decoy.close()
	if err := os.Symlink(filepath.Join(root, "real", "inner"), filepath.Join(root, "link")); err != nil {
		t.Fatalf("create the symbolic link: %v", err)
	}
	spelling := relayReadReview766Spelling(root)
	// The path the command builds keeps the caller's spelling; resolving it is the store's job.
	if got, want := relayReadStorePath(spelling), spelling+"/"+relayReadStoreFile; got != want {
		t.Errorf("relayReadStorePath(%q) = %q, want %q", spelling, got, want)
	}

	projection, err := RelayReadState(context.Background(), spelling, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState(%q): %v", spelling, err)
	}
	if len(projection.Relationships) != 1 {
		t.Fatalf("the projection carries %d relationships, want the one real store's: %+v",
			len(projection.Relationships), projection.Relationships)
	}
	item := relayReadRelationshipByID(t, projection, "rel-CRW-REAL")
	if item.IssueKey != "CRW-REAL" {
		t.Errorf("the read returned %v, so it read the decoy store at %s", item.IssueKey, filepath.Join(root, "state"))
	}
}

// C2: an unresolved state directory is the named error, and the doctor's reason stays reachable.
func TestRelayReadReview766UnresolvedStateIsNamed(t *testing.T) {
	exe, _ := coreFakeCRW(t, "", 3)
	e := dagReviewEnv(t)
	e.Executable = exe
	cfg := &Config{Relay: coreRelay{Socket: "/k0"}}
	projection, err := RelayRead(context.Background(), e, cfg, RelayReadOptions{})
	if !errors.Is(err, ErrRelayStateUnconfigured) {
		t.Fatalf("err = %v, want ErrRelayStateUnconfigured", err)
	}
	var refused *relayHelperUnresolvedError
	if !errors.As(err, &refused) {
		t.Errorf("the doctor's refusal is no longer reachable: %v", err)
	}
	if !strings.Contains(err.Error(), "relay doctor exited with status 3") {
		t.Errorf("the doctor's reason is missing from the error: %v", err)
	}
	if !relayReadEqual(projection, RelayProjection{}) {
		t.Errorf("an unresolved state returned a projection: %+v", projection)
	}
}

// C2 contrast: a context that has already ended is that context's error, not an unconfigured
// state: the two must not be told apart by guesswork.
func TestRelayReadReview766EndedContextIsNotUnconfigured(t *testing.T) {
	exe, _ := coreFakeCRW(t, "", 0)
	e := dagReviewEnv(t)
	e.Executable = exe
	cfg := &Config{Relay: coreRelay{Socket: "/k0"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RelayRead(ctx, e, cfg, RelayReadOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrRelayStateUnconfigured) {
		t.Errorf("a cancelled read is reported as an unconfigured state: %v", err)
	}
}

// C3: an empty value spaced after an option is the same usage error the --name= form gives, and
// it never reaches the relay executable.
func TestRelayReadReview766EmptySpacedValueIsRefused(t *testing.T) {
	for _, option := range []string{"--state", "--plan", "--project"} {
		exe, record := coreFakeCRW(t, "/s1", 0)
		env := dagReviewEnv(t)
		env.Executable = exe
		var stdout, stderr bytes.Buffer
		env.Stdout, env.Stderr = &stdout, &stderr
		code := relayReadCommand.Run(context.Background(), env, []string{option, ""})
		if code != usageExit {
			t.Errorf("%s \"\": exit = %d, want %d (stderr: %s)", option, code, usageExit, stderr.String())
		}
		if !strings.Contains(stderr.String(), option+" needs a value") {
			t.Errorf("%s \"\": the reason is missing: %q", option, stderr.String())
		}
		if !strings.Contains(stderr.String(), "usage: crw manage relay-read") {
			t.Errorf("%s \"\": the usage line is missing: %q", option, stderr.String())
		}
		// The record file is written by the fake on its first call, so an absent file is the
		// strongest form of "the relay was never run".
		switch calls, err := relayReadReview766CallsWhenAny(t, record); {
		case err != nil:
			t.Fatalf("%s \"\": examine the call record: %v", option, err)
		case len(calls) != 0:
			t.Errorf("%s \"\": the relay was run anyway: %q", option, calls)
		}
	}
}

// C4: the marker fixture really puts the verdict marker in the store, so the assertion that the
// projection never carries it is not an empty check.
func TestRelayReadReview766MarkerRowsCarryTheVerdictMark(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadMarkerRows()
	f.close()
	seen := false
	relayReadWithStore(t, f.path, func(ctx context.Context, st *store.Store) {
		rows, err := st.Q(ctx).QueryContext(ctx, "SELECT findings, reason FROM verdict_context WHERE event_id = ?", "evt-1")
		if err != nil {
			t.Fatalf("read the verdict context: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var findings, reason string
			if err := rows.Scan(&findings, &reason); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(findings, relayReadVerdictMark) && strings.Contains(reason, relayReadVerdictMark) {
				seen = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	if !seen {
		t.Fatalf("the fixture carries no verdict marker in verdict_context, so the leak check is vacuous")
	}
	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), relayReadVerdictMark) {
		t.Errorf("the projection carries the verdict marker")
	}
}
