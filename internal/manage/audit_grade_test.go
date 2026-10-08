package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// auditTestEnv is the Env these tests drive: the streams, the host lookups and a fixed
// clock, so a graded_at is a value a test can pin rather than the wall clock.
func auditTestEnv(t *testing.T) (*Env, *strings.Builder, *strings.Builder) {
	t.Helper()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	e := &Env{
		Stdin:  strings.NewReader(""),
		Stdout: out,
		Stderr: errOut,
		Getenv: os.Getenv,
		Now:    func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	return e, out, errOut
}

// auditGraderScript is a fake grader this test drives instead of a real model command. It
// writes the grade.json a run is supposed to leave, or deliberately does not, and the two
// environment values it reads are set by the test that runs it.
const auditGraderScript = "#!/bin/sh\n" +
	"prompt=$1\n" +
	"dir=$(dirname $prompt)\n" +
	"case $AUDIT_FAKE in\n" +
	"json)\n" +
	"printf '%s' \"$AUDIT_JSON\" > $dir/grade.json\n" +
	";;\n" +
	"record)\n" +
	"echo $(pwd) > $AUDIT_RECORD\n" +
	"cat $prompt >> $AUDIT_RECORD\n" +
	"printf '%s' \"$AUDIT_JSON\" > $dir/grade.json\n" +
	";;\n" +
	"barrier)\n" +
	"mkdir -p $AUDIT_BARRIER\n" +
	"echo x >> $AUDIT_BARRIER/joined\n" +
	"i=0\n" +
	"while [ $(wc -l < $AUDIT_BARRIER/joined) -lt $AUDIT_BARRIER_N ]; do\n" +
	"i=$((i+1))\n" +
	"if [ $i -gt 200 ]; then exit 3; fi\n" +
	"sleep 0.05\n" +
	"done\n" +
	"printf '%s' \"$AUDIT_JSON\" > $dir/grade.json\n" +
	";;\n" +
	"span)\n" +
	"echo start $(date +%s%N) >> $AUDIT_SPAN/$$\n" +
	"sleep 0.4\n" +
	"echo end $(date +%s%N) >> $AUDIT_SPAN/$$\n" +
	"printf '%s' \"$AUDIT_JSON\" > $dir/grade.json\n" +
	";;\n" +
	"timeout)\n" +
	"sleep 30\n" +
	";;\n" +
	"nofile)\n" +
	";;\n" +
	"esac\n" +
	"exit 0\n"

// The result documents the fake writes, in the shape crw-audit-result/1 fixes.
const (
	auditJSONWithP1 = "{\"schema\":\"crw-audit-result/1\",\"criteria\":[{\"id\":\"c1\",\"verdict\":\"PASS\",\"note\":\"n\"}],\"defects\":[{\"severity\":\"P1\",\"what\":\"wrong\",\"where\":\"a.go:2\",\"repro\":\"run it\"}],\"score\":6}"
	auditJSONClean  = "{\"schema\":\"crw-audit-result/1\",\"criteria\":[{\"id\":\"c1\",\"verdict\":\"PASS\",\"note\":\"n\"}],\"defects\":[],\"score\":9}"
)

// auditFakeGrader writes the fake and returns the grader command, whose argument list
// uses both placeholders the issue fixes.
func auditFakeGrader(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grader.sh")
	// The script is a program the grader command runs, so its descriptor is open only under
	// syscall.ForkLock: a fork in that window would inherit it and leave the script unexecutable
	// (ETXTBSY, golang/go#22315).
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(path, []byte(auditGraderScript), 0o700)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	return []string{path, "{prompt_file}", "{bundle}"}
}

// auditFake configures the fake grader and the result it should leave.
func auditFake(t *testing.T, mode, result string) []string {
	t.Helper()
	t.Setenv("AUDIT_FAKE", mode)
	t.Setenv("AUDIT_JSON", result)
	return auditFakeGrader(t)
}

// auditLedgerStatuses reads the status of every ledger line under a state directory.
func auditLedgerStatuses(t *testing.T, state string) []string {
	t.Helper()
	rows := auditLines(t, filepath.Join(state, "audit", auditLedgerFile))
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var status string
		if err := json.Unmarshal(row["status"], &status); err != nil {
			t.Fatal(err)
		}
		out = append(out, status)
	}
	return out
}

// C1: a run that leaves a good grade.json is ok, one that leaves no file and one that
// leaves a file the format refuses are both invalid, and one that runs past its time
// limit is timeout. Each is recorded in the ledger, and none is retried.
func TestAuditGradeRecordsOkInvalidAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		result string
		status string
	}{
		{"a good result", "json", auditJSONWithP1, auditStatusOK},
		{"no file", "nofile", "", auditStatusInvalid},
		{"malformed json", "json", "{oops", auditStatusInvalid},
		{"another schema", "json", "{\"schema\":\"crw-audit-result/2\",\"criteria\":[],\"defects\":[],\"score\":5}", auditStatusInvalid},
		{"past the time limit", "timeout", "", auditStatusTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := auditTestEnv(t)
			state := t.TempDir()
			grader := auditFake(t, tc.mode, tc.result)
			cfg := auditSectionConfig(t, state, map[string]any{"grader": grader, "grader_timeout_seconds": 1})
			results, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Status != tc.status {
				t.Fatalf("results = %+v, want one %s", results, tc.status)
			}
			if got := auditLedgerStatuses(t, state); len(got) != 1 || got[0] != tc.status {
				t.Errorf("the ledger says %v, want one %s", got, tc.status)
			}
		})
	}
}

// A ledger line for a run that left no usable result carries a null score and no counts.
func TestAuditGradeLedgerRowForAnInvalidRunHasNoScore(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "nofile", "")})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}}); err != nil {
		t.Fatal(err)
	}
	rows := auditLines(t, filepath.Join(state, "audit", auditLedgerFile))
	if string(rows[0]["score"]) != "null" {
		t.Errorf("score = %s, want null", rows[0]["score"])
	}
	for _, key := range []string{"p0", "p1", "p2", "p3"} {
		if string(rows[0][key]) != "0" {
			t.Errorf("%s = %s, want 0", key, rows[0][key])
		}
	}
}

// C2: a result that carries a P0 or a P1 reaches the alert queue with the caller's
// metadata, and one that does not, does not.
func TestAuditGradeAlertsOnlyTheResultsWithP0OrP1(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", auditJSONWithP1)})
	jobs := []AuditJob{
		{Bundle: auditGoodBundle(t, auditModePR), Pair: "if-deepseek", Phase: "live", Round: "r1"},
		{Bundle: auditGoodBundle(t, auditModePR), Pair: "sol", Phase: "live"},
	}
	if _, err := AuditGrade(context.Background(), e, cfg, jobs); err != nil {
		t.Fatal(err)
	}
	alerts := auditLines(t, filepath.Join(state, "audit", auditAlertFile))
	if len(alerts) != 2 {
		t.Fatalf("the alert queue has %d lines, want one per result", len(alerts))
	}
	var pair string
	if err := json.Unmarshal(alerts[0]["pair"], &pair); err != nil {
		t.Fatal(err)
	}
	if pair != "if-deepseek" {
		t.Errorf("the first alert carries pair %q, want the caller's metadata", pair)
	}
}

// A result with no P0 and no P1 leaves no alert at all, even beside one that does.
func TestAuditGradeDoesNotAlertACleanResult(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", auditJSONClean)})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditAlertFile)); !os.IsNotExist(err) {
		t.Errorf("an alert file was written for a clean result: %v", err)
	}
}

// The caller's metadata reaches the results, and they come back in the order of the jobs
// even though they run at once.
func TestAuditGradeKeepsJobOrderAndCarriesTheMetadata(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", auditJSONClean), "workers": 4})
	jobs := make([]AuditJob, 4)
	for i := range jobs {
		jobs[i] = AuditJob{Bundle: auditGoodBundle(t, auditModePR), Pair: fmt.Sprintf("pair-%d", i), Phase: "live", Round: strconv.Itoa(i)}
	}
	results, err := AuditGrade(context.Background(), e, cfg, jobs)
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		if result.Pair != fmt.Sprintf("pair-%d", i) || result.Round != strconv.Itoa(i) {
			t.Errorf("result %d carries %q/%q, want the job's metadata", i, result.Pair, result.Round)
		}
		if result.Status != auditStatusOK || result.Score != 9 {
			t.Errorf("result %d = %+v, want a clean ok result", i, result)
		}
	}
}

// A configuration with no grader command is refused by name, and nothing is recorded.
func TestAuditGradeRefusesAnUnconfiguredGrader(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	_, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}})
	var unconfigured auditGraderUnconfiguredError
	if !errors.As(err, &unconfigured) {
		t.Fatalf("err = %v, want grader_unconfigured", err)
	}
	if err.Error() != "grader_unconfigured" {
		t.Errorf("err = %q, want the refusal name the command line prints", err)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditLedgerFile)); !os.IsNotExist(err) {
		t.Errorf("a ledger was written for an unconfigured grader: %v", err)
	}
}

// A bundle that cannot be read stops the call before anything is graded.
func TestAuditGradeRefusesAnUnreadableBundle(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", auditJSONClean)})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: filepath.Join(t.TempDir(), "gone")}}); err == nil {
		t.Fatal("an unreadable bundle was accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditLedgerFile)); !os.IsNotExist(err) {
		t.Errorf("a ledger was written for an unreadable bundle: %v", err)
	}
}

// The defaults the issue fixes apply when the section leaves them out.
func TestAuditConfigDefaults(t *testing.T) {
	cfg := auditSectionConfig(t, t.TempDir(), map[string]any{"grader": []string{"x"}})
	section, err := auditConfigOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if section.GraderTimeoutSeconds != auditDefaultTimeoutSeconds || section.Workers != auditDefaultWorkers {
		t.Errorf("defaults = %+v, want %d seconds and %d workers", section, auditDefaultTimeoutSeconds, auditDefaultWorkers)
	}
	section, err = auditConfigOf(&Config{raw: map[string]json.RawMessage{}})
	if err != nil {
		t.Fatal(err)
	}
	if section.Workers != auditDefaultWorkers || len(section.Grader) != 0 {
		t.Errorf("a configuration with no audit section = %+v", section)
	}
}

// The prompt file the grader reads is written into the bundle and the grader runs there,
// which is what the two placeholders and the working directory promise.
func TestAuditGradeWritesThePromptIntoTheBundle(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	record := filepath.Join(t.TempDir(), "calls")
	t.Setenv("AUDIT_RECORD", record)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "record", auditJSONClean)})
	bundle := auditGoodBundle(t, auditModePackage)
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || lines[0] != bundle {
		t.Errorf("the grader ran in %v, want the bundle %q", lines, bundle)
	}
	// The prompt is the mode's own: a package bundle is described by the package layout, not
	// by the pull request layout.
	if !strings.Contains(string(data), auditPkgSrcDir+"/") || !strings.Contains(string(data), auditResultSchema) {
		t.Error("the grader did not read the assembled prompt")
	}
	if _, err := os.Stat(filepath.Join(bundle, auditPromptFile)); err != nil {
		t.Errorf("the prompt was not left in the bundle: %v", err)
	}
}

// C3: the number of graders running at once reaches workers when there is work for them,
// so a barrier of exactly workers graders trips.
func TestAuditGradeRunsTheWorkersTogether(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	t.Setenv("AUDIT_BARRIER", t.TempDir())
	t.Setenv("AUDIT_BARRIER_N", "3")
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "barrier", auditJSONClean), "workers": 3, "grader_timeout_seconds": 60})
	jobs := make([]AuditJob, 3)
	for i := range jobs {
		jobs[i] = AuditJob{Bundle: auditGoodBundle(t, auditModePR)}
	}
	results, err := AuditGrade(context.Background(), e, cfg, jobs)
	if err != nil {
		t.Fatal(err)
	}
	// The barrier only trips when all three graders are alive at once, so a status other
	// than ok here means the engine serialized them.
	for i, result := range results {
		if result.Status != auditStatusOK {
			t.Errorf("result %d = %s, want ok: the workers did not run together", i, result.Status)
		}
	}
}

// C3: with more jobs than workers, the observed concurrency never passes workers, and it
// does reach it, so the cap is a cap and not a serial run.
func TestAuditGradeObservedConcurrencyNeverPassesWorkers(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	span := t.TempDir()
	t.Setenv("AUDIT_SPAN", span)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "span", auditJSONClean), "workers": 2, "grader_timeout_seconds": 60})
	jobs := make([]AuditJob, 6)
	for i := range jobs {
		jobs[i] = AuditJob{Bundle: auditGoodBundle(t, auditModePR)}
	}
	if _, err := AuditGrade(context.Background(), e, cfg, jobs); err != nil {
		t.Fatal(err)
	}
	peak, err := auditPeakConcurrency(span)
	if err != nil {
		t.Fatal(err)
	}
	if peak > 2 {
		t.Errorf("the observed concurrency reached %d, past the 2 workers", peak)
	}
	if peak < 2 {
		t.Errorf("the observed concurrency only reached %d, so the workers never overlapped", peak)
	}
}

// auditPeakConcurrency reads the intervals the span grader recorded and returns the
// largest number that overlap at one instant.
func auditPeakConcurrency(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	type event struct {
		at    int64
		delta int
	}
	var events []event
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			at, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			delta := 1
			if fields[0] == "end" {
				delta = -1
			}
			events = append(events, event{at: at, delta: delta})
		}
	}
	// An end at the same instant as a start closes the earlier run first, so a
	// back-to-back pair is never counted as overlap.
	sort.Slice(events, func(i, j int) bool {
		if events[i].at != events[j].at {
			return events[i].at < events[j].at
		}
		return events[i].delta < events[j].delta
	})
	live, peak := 0, 0
	for _, e := range events {
		live += e.delta
		if live > peak {
			peak = live
		}
	}
	return peak, nil
}

// The time limit is the configured one: a grader that sleeps past it is cut.
func TestAuditGradeHonorsTheConfiguredTimeLimit(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "timeout", ""), "grader_timeout_seconds": 1})
	start := time.Now()
	results, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the run took %s; the grader was not cut at its limit", elapsed)
	}
	if results[0].Status != auditStatusTimeout {
		t.Errorf("status = %s, want timeout", results[0].Status)
	}
}

// C3: a grader that leaves a usable result and then hangs is still a timeout, because it did
// not finish inside its limit and the limit outranks the file it wrote.
func TestAuditGradeKeepsATimeoutDespiteAUsableFile(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	script := filepath.Join(t.TempDir(), "grader.sh")
	body := "#!/bin/sh\n" +
		"dir=$(dirname $1)\n" +
		"printf '%s' \"$AUDIT_JSON\" > $dir/grade.json\n" +
		"sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_JSON", auditJSONClean)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": []string{script, "{prompt_file}"}, "grader_timeout_seconds": 1})
	results, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != auditStatusTimeout {
		t.Errorf("status = %s, want timeout: the time limit must outrank a file left before the hang", results[0].Status)
	}
	if got := auditLedgerStatuses(t, state); len(got) != 1 || got[0] != auditStatusTimeout {
		t.Errorf("the ledger says %v, want one timeout", got)
	}
}

// Two jobs naming one bundle are refused, because they would race over the same grade.json
// and the same prompt rather than each grading their own directory.
func TestAuditGradeRefusesTwoJobsForOneBundle(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", auditJSONClean)})
	bundle := auditGoodBundle(t, auditModePR)
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}, {Bundle: bundle + "/"}}); err == nil {
		t.Fatal("two jobs naming one bundle were accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditLedgerFile)); !os.IsNotExist(err) {
		t.Errorf("a ledger was written for a refused batch: %v", err)
	}
}

// A prompt.md that is a symbolic link is never followed: the link is removed and the prompt
// is written fresh, so a bundle from another process cannot make the audit truncate a file
// outside it.
func TestAuditGradeRefusesASymlinkedPrompt(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := auditGoodBundle(t, auditModePR)
	if err := os.Symlink(target, filepath.Join(bundle, auditPromptFile)); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", auditJSONClean)})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Errorf("the link's target was written through: %q", data)
	}
	info, err := os.Lstat(filepath.Join(bundle, auditPromptFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("the prompt is still a symbolic link")
	}
}

// A noisy grader is bounded: only the head of its output is kept, so it cannot grow the
// audit's memory without end, and the diagnostic line still comes from that head.
func TestAuditGradeBoundsTheCapturedOutput(t *testing.T) {
	log := &auditLog{}
	chunk := bytes.Repeat([]byte("noise\n"), 4096)
	for i := 0; i < 64; i++ {
		if _, err := log.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if log.buf.Len() != auditLogLimit {
		t.Errorf("the log kept %d bytes, want the %d byte limit", log.buf.Len(), auditLogLimit)
	}
	if !strings.HasPrefix(log.String(), "noise\n") {
		t.Errorf("the log did not keep the head of the output: %q", log.String()[:32])
	}
	if first := auditFirstLine(log.String()); first != "noise" {
		t.Errorf("the diagnostic line is %q, want the head of the output", first)
	}
}
