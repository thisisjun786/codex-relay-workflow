package supervisor_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "unsafe"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// sosOperation is one command or store derivation a Python SOS test made: its kind and arguments,
// the pre-N tree it ran on and the clock it ran at.
type sosOperation struct {
	Kind         string         `json:"kind"`
	Args         []any          `json:"args"`
	Pre          string         `json:"pre"`
	Clock        string         `json:"clock"`
	Relationship string         `json:"relationship"`
	Project      string         `json:"project"`
	At           string         `json:"at"`
	Kwargs       map[string]any `json:"kwargs"`
}
type sosCapture struct {
	Operations []sosOperation `json:"operations"`
}

var sosCases = map[string][]string{
	"SOS-1":  {"OnePredicateForBothReaders.test_an_omission_is_the_same_omission_and_the_same_message"},
	"SOS-2":  {"OnePredicateForBothReaders.test_a_declared_in_progress_pause_reads_in_progress_in_both", "OnePredicateForBothReaders.test_a_receipted_readiness_reads_reported_in_both", "OnePredicateForBothReaders.test_readiness_without_a_receipt_is_an_omission_in_both", "OnePredicateForBothReaders.test_a_final_receipt_without_a_declaration_is_owed_by_neither"},
	"SOS-3":  {"OnePredicateForBothReaders.test_a_later_admitted_turn_clears_what_is_owed_in_both", "OnePredicateForBothReaders.test_the_grace_holds_both_and_then_releases_both"},
	"SOS-4":  {"OnePredicateForBothReaders.test_a_registration_for_another_workspace_is_refused_by_both_before_classifying"},
	"SOS-5":  {"AnLLMThatOmitsItsReport.test_an_omitted_report_is_derived_from_the_store_and_sent_once_after_the_grace"},
	"SOS-6":  {"AnLLMThatOmitsItsReport.test_a_declared_in_progress_pause_is_never_sent", "AnLLMThatOmitsItsReport.test_a_later_admitted_turn_clears_the_omission_before_it_is_owed"},
	"SOS-7":  {"AnLLMThatOmitsItsReport.test_a_later_admitted_turn_clears_a_staged_omission_before_transport"},
	"SOS-8":  {"ARegistrationThatIsNotTheClaimedAssignment.test_the_daemon_sends_nothing_for_a_turn_the_marker_reader_cannot_place"},
	"SOS-9":  {"OneLogicalOmissionAcrossRestarts.test_a_restarted_daemon_and_a_redelivered_settlement_converge"},
	"SOS-10": {"ASupervisorWhoCannotBeWokenKeepsTheOmission.test_an_archived_supervisor_is_not_woken_and_the_omission_waits"},
	"SOS-11": {"AParentWhoAlsoStagesTheOmissionByHand.test_the_parent_staging_first_leaves_one_message_and_one_wake"},
	"SOS-12": {"ALegacyAdmissionIsNeverDerived.test_a_child_that_claimed_without_the_store_record_is_never_woken_for"},
	"SOS-13": {"AParentsReadingIsAProposalToo.test_a_declaration_after_the_parent_staged_voids_the_send", "AParentsReadingIsAProposalToo.test_a_declaration_before_the_parent_stages_refuses_the_staging"},
	"SOS-14": {"TheStoreRecordIsReportedNeverDropped.test_a_store_that_cannot_be_written_fails_the_command_after_the_marker_write"},
	"SOS-15": {"TheStoreRecordIsReportedNeverDropped.test_an_intent_naming_no_store_is_reported_and_is_not_a_failure", "TheStoreRecordIsReportedNeverDropped.test_an_intent_that_is_not_an_object_is_reported_rather_than_crashing", "TheStoreRecordIsReportedNeverDropped.test_reporting_derive_creates_no_store_where_none_exists"},
	"SOS-16": {"TheStoreRecordIsReportedNeverDropped.test_both_records_mirror_what_the_marker_stands_on"},
	"SOS-17": {"AMarkerTheMarkerReaderCannotReadRecordsNoCapability.test_a_malformed_marker_fact_records_nothing_and_wakes_nobody", "AMarkerTheMarkerReaderCannotReadRecordsNoCapability.test_an_unreadable_marker_fact_records_nothing_and_wakes_nobody"},
	"SOS-18": {"ADeclarationRacingTheDaemon.test_a_tick_between_the_marker_and_the_store_record_wakes_nobody"},
	"SOS-19": {"ARepairedAdmissionIsOrderedByWhenItWasAdmitted.test_a_legacy_turn_admitted_after_an_omitted_one_clears_it", "ARepairedAdmissionIsOrderedByWhenItWasAdmitted.test_a_repair_inside_the_same_millisecond_is_still_the_later_admission"},
}

// captureSOS restores the tree a Python SOS test left (the former testdata/sos_capture.py): every
// operation's pre-N tree snapshot and capture.json's list of operations. The workspace's marker
// directory is a digest of the workspace's path under root and each receipt's revision hash a
// digest of a manifest naming files under root, so both are derived anew; the golden options it
// returns write them, the tree and the repository back as placeholders.
func captureSOS(t *testing.T, method string) (string, sosCapture, []golden.Option) {
	t.Helper()
	root, err := os.MkdirTemp("", "crw-sos-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	key, err := delivery.WorkspaceKey(filepath.Join(root, "tree", "work"))
	if err != nil {
		t.Fatal(err)
	}
	opts := supervisor.TreeFixtureRevisions(t, method, root, [2]string{key, "<workspace key>"})
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got sosCapture
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	opts = append(opts, golden.Substitute(key, "<workspace key>"))
	return root, got, append(opts, supervisor.TreeGolden(t, root)...)
}

func sosRestore(t *testing.T, root, pre string) {
	t.Helper()
	tree := filepath.Join(root, "tree")
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cp", "-a", pre+"/.", tree)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	// The snapshot's store is a backup copy - a new file beside the mirror the Python run
	// published - and the Go replay reads it as the host that took it over from Python would.
	if db := sosDB(root); db != "" {
		testsupport.Rehome(t, db)
		testsupport.HandOver(t, db, "go")
	}
}
func sosDB(root string) string {
	var found string
	_ = filepath.WalkDir(filepath.Join(root, "tree"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".sqlite3") && filepath.Base(path) != "not-a-store.sqlite3" && found == "" {
			found = path
		}
		return nil
	})
	return found
}
func sosRows(t *testing.T, path string) map[string][]map[string]any {
	t.Helper()
	if path == "" {
		return map[string][]map[string]any{}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return map[string][]map[string]any{}
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return map[string][]map[string]any{}
	}
	tables := map[string][]map[string]any{}
	names, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for names.Next() {
		var n string
		_ = names.Scan(&n)
		list = append(list, n)
	}
	names.Close()
	for _, n := range list {
		rows, err := db.Query("SELECT * FROM " + n + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range vals {
				ptr[i] = &vals[i]
			}
			if err := rows.Scan(ptr...); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				row[cols[i]] = v
			}
			tables[n] = append(tables[n], row)
		}
		rows.Close()
	}
	raw, _ := json.Marshal(tables)
	var normalized map[string][]map[string]any
	_ = json.Unmarshal(raw, &normalized)
	return normalized
}
func sosJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }
func sosValue(v any) any {
	switch value := v.(type) {
	case delivery.Obj:
		out := map[string]any{}
		for _, f := range value {
			out[f.Key] = sosValue(f.Value)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = sosValue(value[i])
		}
		return out
	default:
		return value
	}
}
func sosPlain(v any) map[string]any {
	raw, _ := json.Marshal(sosValue(v))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// sosResult is what one replayed operation answered and the complete store it left.
type sosResult struct {
	answer, tables any
}

func replaySOS(t *testing.T, binary, root string, op sosOperation, result *sosResult) {
	t.Helper()
	sosRestore(t, root, op.Pre)
	var code int
	var output map[string]any
	if op.Kind == "send_window" {
		s, err := store.Open(context.Background(), sosDB(root), "")
		if err != nil {
			t.Fatal(err)
		}
		repo, _ := filepath.Abs("../../..")
		c := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: filepath.Join(repo, ".venv/bin/codex-session-relay"), Settings: &delivery.TaskSettings{}}
		archived, _ := op.Kwargs["archived"].(bool)
		host := &sosHost{archived: archived, nextTurn: int(op.Kwargs["nextTurn"].(float64))}
		previous := sosTokenSource
		sosTokenSource = bytes.NewReader(make([]byte, 64))
		answer, err := c.AutoSend(context.Background(), host, op.Kwargs["now"].(float64), 0, 1, "", "", "")
		sosTokenSource = previous
		if err != nil {
			t.Fatal(err)
		}
		output = sosPlain(answer)
		_ = s.Close()
	} else if op.Kind == "supervisor_step" {
		path := sosDB(root)
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		repo, _ := filepath.Abs("../../..")
		c := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: filepath.Join(repo, ".venv/bin/codex-session-relay")}
		answer, err := c.StageUnsent(context.Background(), op.Project, op.At, 300)
		if err != nil {
			t.Fatal(err)
		}
		output = sosPlain(answer)
		_ = s.Close()
	} else if op.Kind == "derive" {
		output = sosDerive(t, root, op)
	} else {
		args := make([]string, 0, len(op.Args))
		for _, arg := range op.Args {
			args = append(args, fmt.Sprint(arg))
		}
		previous, previousDelivery := sosClockNow, sosDeliveryClock
		if at := sosExpectedClock(op); at != 0 {
			sosClockNow = func() float64 { return at }
			sosDeliveryClock = &delivery.FakeClock{T: at}
		}
		var stdout, stderr strings.Builder
		repo, _ := filepath.Abs("../../..")
		code = cli.ExecuteAs(context.Background(), filepath.Join(repo, ".venv/bin/codex-session-relay"), args, &stdout, &stderr)
		sosClockNow, sosDeliveryClock = previous, previousDelivery
		if stdout.Len() > 0 {
			if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
				t.Fatalf("output %q: %v", stdout.String(), err)
			}
		} else {
			output = map[string]any{}
		}
		if code != 0 && stdout.Len() == 0 {
			t.Logf("command stderr: %s", stderr.String())
		}
	}
	result.answer = map[string]any{"code": code, "output": output}
	got := sosRows(t, sosDB(root))
	for _, row := range got["journal"] {
		if text, ok := row["detail"].(string); ok {
			var parsed any
			if json.Unmarshal([]byte(text), &parsed) == nil {
				row["detail"] = parsed
			}
		}
	}
	result.tables = got
}

// sosDerive is delivery.DeriveOmission on the store of op's restored tree, with op's arguments.
func sosDerive(t *testing.T, root string, op sosOperation) map[string]any {
	t.Helper()
	s, err := store.Open(context.Background(), sosDB(root), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, _ := op.Kwargs["state_directory"].(string)
	if state == "" && len(op.Args) > 0 {
		state, _ = op.Args[0].(string)
	}
	now, _ := op.Kwargs["now"].(string)
	if now == "" && len(op.Args) > 1 {
		now, _ = op.Args[1].(string)
	}
	grace, _ := op.Kwargs["grace"].(float64)
	if grace == 0 && len(op.Args) > 2 {
		grace, _ = op.Args[2].(float64)
	}
	turn, _ := op.Kwargs["turn"].(string)
	return sosPlain(delivery.DeriveOmission(context.Background(), s, state, op.Relationship, turn, now, grace))
}

//go:linkname sosTokenSource github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor.TokenSource
var sosTokenSource io.Reader

type sosHost struct {
	delivery.Adapter
	archived bool
	nextTurn int
}

func (h *sosHost) ReadThread(string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: "idle", CanAcceptInput: &yes}, nil
}
func (h *sosHost) IsArchived(string, any) (*bool, error) { return &h.archived, nil }
func (h *sosHost) ReadGoalStatus(string) (any, error)    { return nil, nil }
func (h *sosHost) SendMessage(id, thread, _ string, _ *delivery.TaskSettings) (delivery.Obj, error) {
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: fmt.Sprintf("turn-%s-%d", thread, h.nextTurn)}}, nil
}

//go:linkname sosClockNow github.com/thisisjun786/codex-relay-workflow/internal/relay/cli.clockNow
var sosClockNow func() float64

//go:linkname sosDeliveryClock github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery.cliClock
var sosDeliveryClock delivery.Clock

// sosExpectedClock is the instant op ran at, 0 when the capture names none. Where the Python run
// named no clock, the capture holds the time the operation recorded (the last turn declaration
// or reporting session it wrote).
func sosExpectedClock(op sosOperation) float64 {
	if at, err := time.Parse("2006-01-02T15:04:05.999999Z07:00", op.Clock); err == nil {
		return float64(at.UnixMicro()) / 1e6
	}
	return 0
}

func testSOSID(t *testing.T, id string) {
	binary := testsupport.CRW(t)
	for _, method := range sosCases[id] {
		method := method
		t.Run(method, func(t *testing.T) {
			root, capture, opts := captureSOS(t, method)
			if len(capture.Operations) == 0 {
				t.Fatal("original Python test exposed no replayable command or store derivation")
			}
			results := make([]*sosResult, len(capture.Operations))
			for i, op := range capture.Operations {
				op := op
				t.Run(string(rune('a'+i)), func(t *testing.T) {
					result := &sosResult{}
					replaySOS(t, binary, root, op, result)
					results[i] = result
				})
			}
			// Each operation's answer and store are compared with the golden in the method's
			// test, so a method keeps one golden file.
			for i, result := range results {
				if result == nil {
					continue
				}
				golden.CheckJSON(t, fmt.Sprintf("%c %s answer", 'a'+i, capture.Operations[i].Kind), result.answer, opts...)
				golden.CheckJSON(t, fmt.Sprintf("%c %s tables", 'a'+i, capture.Operations[i].Kind), result.tables, opts...)
			}
		})
	}
}

// These read-only CLI surfaces run on the Python SOS-5 store, through the built binary as the
// alias the packets name; observedAt is the instant the binary answered.
func Test24_SOS_5_BuiltBinaryBytes(t *testing.T) {
	root, captured, opts := captureSOS(t, sosCases["SOS-5"][0])
	binary := testsupport.CRWAt(t, filepath.Join(t.TempDir(), "crw"))
	alias := filepath.Join(filepath.Dir(binary), "codex-session-relay")
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	// Choose a pre-operation snapshot in which the staged message already exists.
	var op sosOperation
	for _, candidate := range captured.Operations {
		sosRestore(t, root, candidate.Pre)
		if len(sosRows(t, sosDB(root))["supervisor_messages"]) > 0 {
			op = candidate
			break
		}
	}
	if op.Pre == "" {
		t.Fatal("SOS-5 captured no staged omission")
	}
	sosRestore(t, root, op.Pre)
	rows := sosRows(t, sosDB(root))
	messages := rows["supervisor_messages"]
	if len(messages) == 0 {
		t.Fatal("no frozen omission snapshot")
	}
	state := filepath.Dir(sosDB(root))
	env := append(os.Environ(), "HOME="+filepath.Join(root, "home"), "XDG_STATE_HOME="+filepath.Join(root, "home/state"), "CODEX_HOME="+filepath.Join(root, "home/codex"))
	for _, args := range [][]string{{"supervisor-show", "--message", messages[0]["message_id"].(string)}, {"reporting-derive", "--relationship", messages[0]["relationship_id"].(string), "--grace", "0"}} {
		t.Run(args[0], func(t *testing.T) {
			testsupport.HandOver(t, sosDB(root), "go")
			cmd := exec.Command(alias, append([]string{"--state", state}, args...)...)
			cmd.Env = env
			got, goErr := cmd.CombinedOutput()
			var answer map[string]any
			if err := json.Unmarshal(got, &answer); err != nil {
				t.Fatalf("Go JSON: %v %s", err, got)
			}
			at, _ := answer["observedAt"].(string)
			golden.CheckJSON(t, args[0], map[string]any{"code": sosExit(t, goErr), "output": string(got)}, append([]golden.Option{golden.Substitute(alias, "<alias>"), golden.Substitute(at, "<observedAt>")}, opts...)...)
		})
	}
}

// sosExit is the exit code of a command that ended with err.
func sosExit(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	e, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	return e.ExitCode()
}

func Test24_ReportingShowCommandContext(t *testing.T) {
	root, captured, _ := captureSOS(t, sosCases["SOS-5"][0])
	// The first derivation that finds the report owed, on its restored snapshot.
	var reading map[string]any
	for _, op := range captured.Operations {
		if op.Kind != "derive" {
			continue
		}
		sosRestore(t, root, op.Pre)
		if derived := sosDerive(t, root, op); derived["owed"] == true {
			sosRestore(t, root, op.Pre)
			reading = derived
			break
		}
	}
	if reading == nil {
		t.Fatal("SOS-5 produced no owed observation")
	}
	selectors := reading["selectors"].(map[string]any)
	command, ok := dispatch.Lookup("reporting-show")
	if !ok {
		t.Fatal("reporting-show is not registered")
	}
	args := []string{}
	for _, pair := range [][2]string{{"marker-root", "markerRoot"}, {"workspace", "workspace"}, {"assignment", "assignment"}, {"session", "session"}, {"turn", "turn"}} {
		args = append(args, "--"+pair[0], selectors[pair[1]].(string))
	}
	parsed := argparse.Parse("reporting-show", args)
	if parsed.Message != "" {
		t.Fatal(parsed.Message)
	}
	selection := store.StateSelection{Path: selectors["state"].(string), Source: "flag"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	previous := sosClockNow
	sosClockNow = func() float64 { return 1700001000 }
	defer func() { sosClockNow = previous }()
	got, err := command.Run(ctx, dispatch.Services{Selection: selection}, dispatch.Args{Parsed: parsed})
	if err != nil {
		t.Fatal(err)
	}
	want := delivery.ObserveOmission(ctx, selection, selectors["markerRoot"].(string), selectors["workspace"].(string), selectors["assignment"].(string), selectors["session"].(string), selectors["turn"].(string), delivery.ISOOf(sosClockNow()), 0)
	live := delivery.ObserveOmission(context.Background(), selection, selectors["markerRoot"].(string), selectors["workspace"].(string), selectors["assignment"].(string), selectors["session"].(string), selectors["turn"].(string), delivery.ISOOf(sosClockNow()), 0)
	if reflect.DeepEqual(want, live) {
		t.Fatal("fixture did not reach the context-sensitive store read")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("command context comparison diff\nGo: %s\nCanceled observation: %s", sosJSON(got), sosJSON(want))
	}
}

func Test24_ObservationFilesBuiltBinaryBytes(t *testing.T) {
	binary := testsupport.CRW(t)
	root := t.TempDir()
	env := append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
	// The commands answer on this one state directory in turn; the first to open the store
	// creates it, and each is handed the store before it answers.
	db := filepath.Join(root, "state", "relay.sqlite3")
	for _, tc := range []struct{ name, data string }{{"missing", ""}, {"directory", ""}, {"list", "[]"}, {"truncated", "{\"a\":"}, {"invalid", "not json"}, {"utf8", string([]byte{0xff})}} {
		path := filepath.Join(root, tc.name)
		if tc.name == "directory" {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		} else if tc.name != "missing" {
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		for _, command := range []string{"supervisor-standing", "supervisor-stage", "supervisor-report-recorded"} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				args := []string{"--state", filepath.Join(root, "state"), command, "--observation", path}
				if command == "supervisor-standing" {
					args = append(args, "--project", "PRJ-1")
				}
				if _, err := os.Stat(db); err == nil {
					testsupport.HandOver(t, db, "go")
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				goCmd := exec.Command(binary, append([]string{"relay"}, args...)...)
				goCmd.Env = env
				got, goErr := goCmd.CombinedOutput()
				golden.CheckJSON(t, "answer", map[string]any{"code": sosExit(t, goErr), "output": string(got)}, supervisor.TreeGolden(t, root)...)
			})
		}
	}
}

func Test24_SOS_1_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-1") }
func Test24_SOS_2_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-2") }
func Test24_SOS_3_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-3") }
func Test24_SOS_4_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-4") }
func Test24_SOS_5_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-5") }
func Test24_SOS_6_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-6") }
func Test24_SOS_7_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-7") }
func Test24_SOS_8_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-8") }
func Test24_SOS_9_WholeOutputAndSQLite(t *testing.T)  { testSOSID(t, "SOS-9") }
func Test24_SOS_10_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-10") }
func Test24_SOS_11_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-11") }
func Test24_SOS_12_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-12") }
func Test24_SOS_13_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-13") }
func Test24_SOS_14_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-14") }
func Test24_SOS_15_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-15") }
func Test24_SOS_16_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-16") }
func Test24_SOS_17_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-17") }
func Test24_SOS_18_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-18") }
func Test24_SOS_19_WholeOutputAndSQLite(t *testing.T) { testSOSID(t, "SOS-19") }
