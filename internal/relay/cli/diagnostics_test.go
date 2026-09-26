package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// scenario is one test_diagnostics.py scenario as the real Python relay left it: its store,
// the FakeClock instant, and Python's own `status` output for that store at that instant.
type scenario struct {
	State          string   `json:"state"`
	Now            float64  `json:"now"`
	Argv           []string `json:"argv"`
	Stdout         string   `json:"stdout"`
	Program        string   `json:"program"`
	RelationshipID string   `json:"relationshipId"`
}

// shown is Python's `show` for one event of a scenario store.
type shown struct {
	State  string   `json:"state"`
	Argv   []string `json:"argv"`
	Code   int      `json:"code"`
	Stdout string   `json:"stdout"`
}

type recorded struct {
	Status map[string]scenario `json:"status"`
	Show   map[string]shown    `json:"show"`
}

var (
	scenariosOnce sync.Once
	scenarios     recorded
	scenariosErr  error
	scenariosDir  string
)

// TestMain removes the directory the Python scenarios were built in, whatever the tests did.
func TestMain(m *testing.M) {
	code := m.Run()
	if scenariosDir != "" {
		if err := os.RemoveAll(scenariosDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

// pythonScenarios runs testdata/diagnostics_scenarios.py once per package run: every Python
// test in test_diagnostics.py executes with its own assertions, and its store is kept.
func pythonScenarios(t *testing.T) map[string]scenario { return pythonRecorded(t).Status }

func pythonRecorded(t *testing.T) recorded {
	t.Helper()
	scenariosOnce.Do(func() {
		root := repositoryRoot(t)
		dir, err := os.MkdirTemp("", "crw20-diagnostics-")
		if err != nil {
			scenariosErr = err
			return
		}
		scenariosDir = dir
		home := filepath.Join(dir, "home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			scenariosErr = err
			return
		}
		script, err := filepath.Abs("testdata/diagnostics_scenarios.py")
		if err != nil {
			scenariosErr = err
			return
		}
		command := exec.Command("uv", "run", "--no-sync", "--project", root, "python", script, dir)
		command.Dir = filepath.Join(root, "packages", "codex-session-relay")
		command.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(dir, "xdg-state"),
			"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(dir, "xdg-data"),
			"XDG_CACHE_HOME="+filepath.Join(dir, "xdg-cache"), "CODEX_HOME="+filepath.Join(dir, "codex-home"),
			"PYTHONPATH="+command.Dir, "CODEX_SESSION_RELAY_STATE=", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY=")
		var stderr strings.Builder
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			scenariosErr = err
			if stderr.Len() > 0 {
				scenariosErr = &scriptError{err, stderr.String()}
			}
			return
		}
		scenariosErr = json.Unmarshal(output, &scenarios)
	})
	if scenariosErr != nil {
		t.Fatalf("Python scenarios: %v", scenariosErr)
	}
	return scenarios
}

type scriptError struct {
	err    error
	stderr string
}

func (e *scriptError) Error() string { return e.err.Error() + "\n" + e.stderr }

// compareStatus runs Go's status on the store Python left and compares the whole JSON.
func compareStatus(t *testing.T, name string) map[string]any {
	t.Helper()
	found, ok := pythonScenarios(t)[name]
	if !ok {
		t.Fatalf("no Python scenario %q", name)
	}
	restore := cli.SetClock(found.Now)
	defer restore()
	got := golang(t, filepath.Dir(found.State), append([]string{"--state", found.State, "status"}, found.Argv...)...)
	// The one intended difference: a recovery line names the relay that renders it, which is
	// Python's console script there and this binary here. The expected Go prefix is computed
	// here, independently of the code under test: this process's resolved executable + "relay".
	want := strings.ReplaceAll(found.Stdout, found.Program, strings.Join(expectedProgram(t), " "))
	if got.code != 0 || got.stdout != want {
		t.Fatalf("exit %d\npython:\n%s\ngo:\n%s", got.code, want, got.stdout)
	}
	return decode(t, got.stdout)
}

// expectedProgram is what relay_program() means for a crw with no codex-session-relay link
// beside it: the resolved executable, then the relay mode.
func expectedProgram(t *testing.T) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if executable, err = filepath.EvalSymlinks(executable); err != nil {
		t.Fatal(err)
	}
	return []string{executable, "relay"}
}

func deliveryOf(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	deliveries := status["deliveries"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries %v", deliveries)
	}
	return deliveries[0].(map[string]any)
}

func healthOf(status map[string]any) map[string]any { return status["observation"].(map[string]any) }

func anchorOf(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	anchors := healthOf(status)["anchors"].(map[string]any)
	if len(anchors) != 1 {
		t.Fatalf("anchors %v", anchors)
	}
	for _, anchor := range anchors {
		return anchor.(map[string]any)
	}
	return nil
}

func requireEqual(t *testing.T, got, want any, what string) {
	t.Helper()
	if !jsonEqual(got, want) {
		t.Fatalf("%s: got %v want %v", what, got, want)
	}
}

func jsonEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

// P01-P13: status phases, each compared whole with Python and then the property itself.
func TestStatus_phases_match_python(t *testing.T) {
	phases := map[string]string{
		"test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt":              "awaiting_send",
		"test_a_delivery_whose_send_is_in_flight_says_so":                            "in_flight",
		"test_a_busy_parent_is_named_as_such_with_its_next_retry":                    "parent_busy",
		"test_a_dispatched_delivery_is_awaiting_acknowledgement":                     "awaiting_ack",
		"test_a_dispatched_completion_still_awaits_its_acknowledgement":              "awaiting_ack",
		"test_a_closed_channel_is_queryable_rather_than_hidden":                      "channel_closed",
		"test_a_refused_turn_start_is_not_evidence_that_a_turn_exists":               "outcome_unknown",
		"test_a_started_turn_whose_answer_was_lost_is_still_turn_accepted":           "turn_accepted",
		"test_an_ordinary_resume_failure_is_not_called_a_settings_rejection":         "withheld:thread/resume",
		"test_a_real_settings_rejection_still_says_so":                               "settings_rejected",
		"test_a_verified_rejection_is_not_reported_as_awaiting_a_receipt":            "rejected",
		"test_a_verified_acceptance_still_reports_acknowledged":                      "acknowledged",
		"test_an_unverified_acknowledgement_settles_nothing":                         "awaiting_ack",
		"test_missing_settings_are_named_rather_than_reported_as_awaiting_a_receipt": "withheld:settings_check",
	}
	for name, want := range phases {
		t.Run(name, func(t *testing.T) {
			item := deliveryOf(t, compareStatus(t, name))
			requireEqual(t, item["phase"], want, "phase")
			switch name {
			case "test_a_busy_parent_is_named_as_such_with_its_next_retry":
				if item["nextRetryAt"] == nil {
					t.Fatal("an operator needs to know when, too")
				}
				requireEqual(t, item["lastFailedOperation"].(map[string]any)["operation"], "parent_busy", "operation")
			case "test_a_closed_channel_is_queryable_rather_than_hidden":
				requireEqual(t, item["lastFailedOperation"].(map[string]any)["error_code"], "unsupported_approval_policy", "error_code")
			case "test_missing_settings_are_named_rather_than_reported_as_awaiting_a_receipt":
				requireEqual(t, item["state"], "withheld_pre_send", "state")
				requireEqual(t, item["attempts"], 0.0, "attempts")
				failure := item["lastFailedOperation"].(map[string]any)
				requireEqual(t, failure["operation"], "settings_check", "operation")
				if failure["next_retry_at"] == nil {
					t.Fatal("next_retry_at")
				}
			}
		})
	}
}

// P06 settings_check failure carries every mismatched field; P07 it survives reopening.
func TestStatus_settings_rejection_and_reopen_match_python(t *testing.T) {
	t.Run("test_a_settings_rejection_records_the_field_the_host_disagreed_on", func(t *testing.T) {
		failure := deliveryOf(t, compareStatus(t, "test_a_settings_rejection_records_the_field_the_host_disagreed_on"))["lastFailedOperation"].(map[string]any)
		requireEqual(t, failure["operation"], "settings_check", "operation")
		difference := failure["difference"].(string)
		for _, field := range []string{"approvalPolicy", "onRequest", "reasoningEffort"} {
			if !strings.Contains(difference, field) {
				t.Fatalf("difference %q lacks %s", difference, field)
			}
		}
	})
	t.Run("test_the_diagnosis_survives_reopening_the_database", func(t *testing.T) {
		// The Python test closed its store; Go reopens the file and still reads the diagnosis.
		if deliveryOf(t, compareStatus(t, "test_the_diagnosis_survives_reopening_the_database"))["lastFailedOperation"] == nil {
			t.Fatal("failed_operations did not survive reopening")
		}
	})
}

// P11 refused-before-queue intents are listed, and a scoped status filters them.
func TestStatus_pending_intents_match_python(t *testing.T) {
	t.Run("test_an_event_refused_at_the_queue_is_visible_in_status", func(t *testing.T) {
		status := compareStatus(t, "test_an_event_refused_at_the_queue_is_visible_in_status")
		requireEqual(t, status["deliveries"], []any{}, "deliveries")
		intent := status["pendingIntents"].([]any)[0].(map[string]any)
		requireEqual(t, intent["phase"], "refused_pre_queue", "phase")
		if !strings.Contains(intent["lastError"].(string), "relationship_not_active") || intent["nextRetryAt"] == nil {
			t.Fatalf("intent %v", intent)
		}
	})
	t.Run("test_a_scoped_status_filters_the_pending_intents_too", func(t *testing.T) {
		requireEqual(t, compareStatus(t, "test_a_scoped_status_filters_the_pending_intents_too")["pendingIntents"], []any{}, "pendingIntents")
	})
}

// P13 a dispatched revision awaits the child's receipt, not an acknowledgement.
func TestStatus_revision_phase_matches_python(t *testing.T) {
	status := compareStatus(t, "test_a_dispatched_revision_is_not_waiting_for_an_acknowledgement")
	for _, one := range status["deliveries"].([]any) {
		item := one.(map[string]any)
		if item["kind"] == "revision_request" {
			requireEqual(t, item["state"], "dispatched", "state")
			requireEqual(t, item["phase"], "awaiting_child_receipt", "phase")
			return
		}
	}
	t.Fatal("no revision delivery")
}

// P14-P23: observation health, compared whole with Python at the scenario's FakeClock instant.
func TestStatus_observation_health_matches_python(t *testing.T) {
	cases := map[string]func(t *testing.T, status map[string]any){
		"test_a_live_loop_with_nothing_polled_is_not_healthy": func(t *testing.T, status map[string]any) {
			anchor := anchorOf(t, status)
			if anchor["lastPolledAt"] != nil || anchor["lastError"] == nil {
				t.Fatalf("anchor %v", anchor)
			}
			requireEqual(t, healthOf(status)["health"], "stalled", "health")
		},
		"test_an_anchor_the_scheduler_has_not_reached_is_not_reported_healthy": func(t *testing.T, status map[string]any) {
			requireEqual(t, anchorOf(t, status)["lastPolledAt"], nil, "lastPolledAt")
			requireEqual(t, healthOf(status)["health"], "stalled", "health")
		},
		"test_a_successful_poll_then_a_failure_keeps_the_last_success": func(t *testing.T, status map[string]any) {
			anchor := anchorOf(t, status)
			if anchor["lastPolledAt"] == nil || anchor["lastError"] == nil {
				t.Fatalf("anchor %v", anchor)
			}
		},
		"test_a_staged_backlog_is_visible_with_its_age": func(t *testing.T, status map[string]any) {
			health := healthOf(status)
			if len(health["stagedEvents"].([]any)) != 1 || health["oldestStagedAgeSeconds"].(float64) <= 3600 {
				t.Fatalf("health %v", health)
			}
			for _, n := range health["backlog"].(map[string]any) {
				requireEqual(t, n, 1.0, "backlog")
			}
			if !strings.Contains(health["note"].(string), "liveness") {
				t.Fatal("note")
			}
		},
		"test_a_finished_quiet_assignment_does_not_age_into_a_false_alarm": func(t *testing.T, status map[string]any) {
			anchor := anchorOf(t, status)
			if anchor["settled"] != true || anchor["ageSeconds"].(float64) <= 900 {
				t.Fatalf("anchor %v", anchor)
			}
			requireEqual(t, healthOf(status)["health"], "healthy", "health")
		},
		"test_a_late_staged_receipt_reopens_the_same_anchor": func(t *testing.T, status map[string]any) {
			requireEqual(t, anchorOf(t, status)["settled"], false, "settled")
			requireEqual(t, healthOf(status)["health"], "stalled", "health")
		},
		"test_each_assignment_settles_a_shared_turn_for_itself": func(t *testing.T, status map[string]any) {
			anchors := healthOf(status)["anchors"].(map[string]any)
			if len(anchors) != 2 {
				t.Fatalf("anchors %v", anchors)
			}
			for _, anchor := range anchors {
				requireEqual(t, anchor.(map[string]any)["settled"], true, "settled")
			}
			requireEqual(t, healthOf(status)["health"], "healthy", "health")
		},
		"test_another_assignments_staged_work_does_not_unsettle_this_one": func(t *testing.T, status map[string]any) {
			settled := 0
			for _, anchor := range healthOf(status)["anchors"].(map[string]any) {
				if anchor.(map[string]any)["settled"] == true {
					settled++
				}
			}
			if settled != 1 {
				t.Fatalf("the observed assignment must stay settled: %v", healthOf(status)["anchors"])
			}
		},
		"test_a_cancelled_assignments_staged_event_does_not_hold_health_down": func(t *testing.T, status map[string]any) {
			requireEqual(t, healthOf(status)["stagedEvents"], []any{}, "stagedEvents")
			requireEqual(t, healthOf(status)["health"], "healthy", "health")
		},
		"test_backlog_and_staged_events_agree_after_an_assignment_is_cancelled": func(t *testing.T, status map[string]any) {
			requireEqual(t, healthOf(status)["stagedEvents"], []any{}, "stagedEvents")
			requireEqual(t, healthOf(status)["backlog"], map[string]any{}, "backlog")
		},
		"test_a_generation_whose_anchor_is_not_bound_yet_is_not_a_stall": func(t *testing.T, status map[string]any) {
			anchor := anchorOf(t, status)
			if anchor["anchorPending"] != true || anchor["turnId"] != nil {
				t.Fatalf("anchor %v", anchor)
			}
			requireEqual(t, healthOf(status)["health"], "healthy", "health")
		},
		"test_an_unbound_anchor_becomes_pollable_once_it_binds": func(t *testing.T, status map[string]any) {
			requireEqual(t, anchorOf(t, status)["anchorPending"], false, "anchorPending")
			requireEqual(t, healthOf(status)["health"], "stalled", "health")
		},
	}
	for name, check := range cases {
		t.Run(name, func(t *testing.T) { check(t, compareStatus(t, name)) })
	}
}

// P19 opening a store backfills assignment_settlements from observations.
func TestStatus_test_an_upgraded_store_does_not_forget_what_it_had_already_settled(t *testing.T) {
	found := pythonScenarios(t)["test_an_upgraded_store_does_not_forget_what_it_had_already_settled"]
	restore := cli.SetClock(found.Now)
	defer restore()
	// The Go build opens the upgraded file; the anchor it settled before is settled again.
	status := golang(t, filepath.Dir(found.State), "--state", found.State, "status")
	if status.code != 0 {
		t.Fatalf("status %+v", status)
	}
	anchor := healthOf(decode(t, status.stdout))["anchors"].(map[string]any)[found.RelationshipID].(map[string]any)
	requireEqual(t, anchor["settled"], true, "settled")
	requireEqual(t, anchor["turnId"], "turn-dispatch-1", "turnId")
}

// show over every event the scenarios left: the whole JSON, byte for byte, including the
// frozen attempt messages, and the usage refusal for an unknown event.
func TestShow_matches_python_on_every_scenario_event(t *testing.T) {
	shows := pythonRecorded(t).Show
	if len(shows) < 20 {
		t.Fatalf("only %d show scenarios", len(shows))
	}
	messages := 0
	for name, want := range shows {
		t.Run(name, func(t *testing.T) {
			got := golang(t, filepath.Dir(want.State), append([]string{"--state", want.State, "show"}, want.Argv...)...)
			if got.code != want.Code || got.stdout != want.Stdout {
				t.Fatalf("exit python=%d go=%d\npython:\n%s\ngo:\n%s", want.Code, got.code, want.Stdout, got.stdout)
			}
		})
		if strings.HasSuffix(name, "/message") {
			messages++
		}
	}
	for _, name := range []string{"preview-revision", "preview-completion"} {
		if shows[name].Code != 0 || !strings.Contains(shows[name].Stdout, `"previewMessage"`) || !strings.Contains(shows[name].Stdout, `"attemptMessages": []`) {
			t.Fatalf("%s: %+v", name, shows[name])
		}
	}
	if messages == 0 || shows["unknown-event"].Code != 4 {
		t.Fatalf("messages=%d unknown=%+v", messages, shows["unknown-event"])
	}
}
