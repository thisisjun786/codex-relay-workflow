package delivery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-505: the way out of a contradictory_observation refusal of an execution-only emit, followed
// through the built `crw relay`. A child that ends its work with `emit --outcome failed` (or
// interrupted) and leaves out --turn-status takes the default, inProgress, which cannot carry
// either outcome. The refusal used to state only that rule, the child read it as "receipt not
// emitted", ended its turn, and nothing reached its parent. The refusal now names the status to
// pass, and the same emit with it is accepted. The rule, the reason and the exit code are the
// same, so an explicitly stated inProgress is still refused (and told the same way).

func turnStatusSide(t *testing.T) (*cliSide, string) {
	t.Helper()
	side := newSide(t, filepath.Join(t.TempDir(), "work"))
	return side, strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
}

func TestCRW505_a_failed_or_interrupted_emit_without_a_turn_status_is_told_which_one_to_pass(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"failed", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			side, rid := turnStatusSide(t)
			events := func() string { return sqliteDump(t, side, "SELECT count(*) FROM events") }
			base := []string{"emit", "--relationship", rid, "--generation", "1", "--outcome", outcome, "--turn-thread", child, "--turn-id", dispatchTurn}
			refusals := 0
			// The child leaves the flag out (the form the packet used to give), and a child that
			// states the default itself is refused alike: the rule is the same for both.
			for _, form := range []struct {
				name   string
				status []string
			}{{name: "left out"}, {name: "stated inProgress", status: []string{"--turn-status", "inProgress"}}} {
				refused, code := runJSON(t, side, append(append([]string(nil), base...), form.status...)...)
				detail, _ := refused["detail"].(string)
				if code != 2 || refused["reason"] != "contradictory_observation" {
					t.Fatalf("%s, --turn-status %s: %d %v", outcome, form.name, code, refused)
				}
				if !strings.Contains(detail, "cannot carry an outcome of") {
					t.Fatalf("the refusal no longer states the rule: %q", detail)
				}
				if !strings.Contains(detail, "--turn-status "+outcome) {
					t.Fatalf("the refusal does not name the status to pass, --turn-status %s:\n%s", outcome, detail)
				}
				help := argparse.Help("crw relay", "emit")
				for _, flag := range flagToken.FindAllString(detail, -1) {
					if !strings.Contains(help, flag) {
						t.Fatalf("the refusal names %s, which emit does not take:\n%s", flag, help)
					}
				}
				refusals++
				if got := events(); got != "[[0]]\n" {
					t.Fatalf("a refused emit stored %s receipt events", got)
				}
				if got := sqliteDump(t, side, "SELECT count(*) FROM refusals WHERE reason = 'contradictory_observation'"); got != fmt.Sprintf("[[%d]]\n", refusals) {
					t.Fatalf("the refusal ledger holds %s contradictory_observation rows after %d refusals", got, refusals)
				}
			}

			// The correction the refusal names: the same emit with that status is accepted, and as
			// the status is the child's claim of how its turn ended the receipt is final.
			accepted, code := runJSON(t, side, append(append([]string(nil), base...), "--turn-status", outcome)...)
			receipt, _ := accepted["receipt"].(map[string]any)
			if code != 0 || receipt["outcome"] != outcome || receipt["revisionHash"] != strings.Repeat("0", 64) || receipt["manifest"] != nil {
				t.Fatalf("the emit with --turn-status %s: %d %v", outcome, code, accepted)
			}
			if accepted["stage"] != "final" || accepted["observedTurnStatus"] != outcome || accepted["terminalProof"] != "claimed" || accepted["delivery"] == nil {
				t.Fatalf("the accepted receipt is not the claimed, final and queued one: %v", accepted)
			}
			if got := events(); got != "[[1]]\n" {
				t.Fatalf("the corrected emit stored %s receipt events", got)
			}
		})
	}
}

// What the hint does not touch: blocked_needs_input without --turn-status keeps staging from the
// child's live turn, and an outcome the refusal gives no instruction for is not told one.
func TestCRW505_the_other_outcomes_keep_their_behaviour(t *testing.T) {
	t.Parallel()
	t.Run("blocked_needs_input without a turn status is staged", func(t *testing.T) {
		t.Parallel()
		side, rid := turnStatusSide(t)
		accepted, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", child, "--turn-id", dispatchTurn)
		receipt, _ := accepted["receipt"].(map[string]any)
		if code != 0 || receipt["outcome"] != "blocked_needs_input" || accepted["stage"] != "staged" || accepted["observedTurnStatus"] != "inProgress" || accepted["delivery"] != nil {
			t.Fatalf("blocked_needs_input without --turn-status: %d %v", code, accepted)
		}
	})
	t.Run("blocked_needs_input on a failed turn is refused without an instruction", func(t *testing.T) {
		t.Parallel()
		side, rid := turnStatusSide(t)
		refused, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", child, "--turn-id", dispatchTurn, "--turn-status", "failed")
		detail, _ := refused["detail"].(string)
		if code != 2 || refused["reason"] != "contradictory_observation" || strings.Contains(detail, "--turn-status") {
			t.Fatalf("blocked_needs_input with --turn-status failed: %d %v", code, refused)
		}
	})
}

// Under --socket the relay reads the turn status from the host and --turn-status is not read, so
// the instruction that works from there is to emit without --socket. Not parallel: it replaces
// the package's ObserveTurn.
// sequential: assigns the package variable ObserveTurn.
func TestCRW505_under_a_socket_the_refusal_says_to_emit_without_it(t *testing.T) {
	side, rid := turnStatusSide(t)
	previous := ObserveTurn
	ObserveTurn = func(context.Context, string, string, string, string) (string, error) { return "inProgress", nil }
	t.Cleanup(func() { ObserveTurn = previous })
	registered, ok := dispatch.Lookup("emit")
	if !ok {
		t.Fatal("emit is not registered")
	}
	parsed := argparse.Parse("emit", []string{"--relationship", rid, "--generation", "1", "--outcome", "failed", "--turn-thread", child, "--turn-id", dispatchTurn, "--turn-status", "failed"})
	if parsed.Message != "" {
		t.Fatal(parsed.Message)
	}
	run := &cliRun{command: "emit", ctx: context.Background(), args: argsOf("emit", dispatch.Args{Parsed: parsed, Defaults: registered.Defaults}), state: side.state, socket: filepath.Join(t.TempDir(), "host.sock"), clock: cliClock}
	defer func() {
		if run.store != nil {
			_ = run.store.Close()
		}
	}()
	_, err := cmdEmit(run)
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "contradictory_observation" {
		t.Fatalf("a failed emit over a live turn the host reports inProgress: %v", err)
	}
	for _, want := range []string{"cannot carry an outcome of", "without --socket", "--state", "--turn-status failed"} {
		if !strings.Contains(refused.Detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, refused.Detail)
		}
	}
	if strings.Contains(refused.Detail, "defaults to") {
		t.Fatalf("the refusal tells a --socket emit about the flag's default, which --socket ignores:\n%s", refused.Detail)
	}
	global := argparse.Help("crw relay", "")
	for _, flag := range flagToken.FindAllString(refused.Detail, -1) {
		if !strings.Contains(global, flag) && !strings.Contains(argparse.Help("crw relay", "emit"), flag) {
			t.Fatalf("the refusal names %s, which no command line takes:\n%s", flag, refused.Detail)
		}
	}
}
