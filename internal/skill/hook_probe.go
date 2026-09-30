package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func asObject(v any) hook.Object { o, _ := v.(contract.OrderedObject); return o }
func objGet(o hook.Object, k string) any {
	for _, f := range o {
		if f.Key == k {
			return f.Value
		}
	}
	return nil
}
func objSet(o hook.Object, k string, v any) hook.Object {
	for i := range o {
		if o[i].Key == k {
			o[i].Value = v
			return o
		}
	}
	return append(o, contract.Field{Key: k, Value: v})
}
func orderedPlain(v any) any {
	switch x := v.(type) {
	case contract.OrderedObject:
		m := map[string]any{}
		for _, f := range x {
			m[f.Key] = orderedPlain(f.Value)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = orderedPlain(x[i])
		}
		return out
	default:
		return x
	}
}
func parseMoment(v any) *time.Time   { return delivery.Moment(v) }
func named(v any) bool               { s, ok := v.(string); return ok && strings.TrimSpace(s) != "" }
func claimant(claim hook.Object) any { return claimantTrace(claim, nil) }
func claimantTrace(claim hook.Object, reached *hookReplayReach) any {
	fact, _ := objGet(claim, "factId").(string)
	parts := strings.Split(fact, "/")
	// hook_probe._claimant: the owning directory (2 parts) or the file inside it (3 parts).
	shaped := len(parts) >= 2 && len(parts) <= 3 && parts[0] == "claims" && (len(parts) == 2 || parts[2] == "claim.json")
	if !shaped || parts[1] == "." || parts[1] == ".." {
		if !shaped {
			markHookReplay(reached, "_claimant", 1)
		} else {
			markHookReplay(reached, "_claimant", 2)
		}
		return nil
	}
	if body := objGet(claim, "sessionId"); !named(body) || body != parts[1] {
		markHookReplay(reached, "_claimant", 3)
		return nil
	}
	markHookReplay(reached, "_claimant", 4)
	return parts[1]
}
func selectingClaimTrace(marker hook.Object, session, assignment any, reached *hookReplayReach) bool {
	items, _ := objGet(marker, "claims").([]any)
	for _, raw := range items {
		c := asObject(raw)
		if c == nil {
			continue
		}
		if !delivery.SameIdentity(claimantTrace(c, reached), session) {
			continue
		}
		p, _ := objGet(c, "dispatchRequestId").(string)
		if strings.TrimSpace(p) == "" {
			continue
		}
		sum := sha256.Sum256([]byte(p))
		if hex.EncodeToString(sum[:]) != assignment {
			continue
		}
		intent := asObject(objGet(marker, "intent"))
		// hook_probe._selecting_claim: only a READABLE intent (every identity slot
		// present is a string) can contradict the directory.
		readable := intent != nil
		for _, field := range probeIdentities["intent"] {
			if _, text := objGet(intent, field).(string); objHas(intent, field) && !text {
				readable = false
			}
		}
		decl := objGet(intent, "dispatchRequestIdHash")
		if readable && named(decl) && decl != assignment {
			continue
		}
		return true
	}
	return false
}
func obstructedClaim(marker hook.Object, session any) bool {
	items, _ := objGet(marker, "claims").([]any)
	for _, raw := range items {
		claim := asObject(raw)
		if claim == nil {
			continue
		}
		if !delivery.SameIdentity(claimant(claim), session) {
			continue
		}
		for _, field := range claim {
			if field.Key == "dispatchRequestId" {
				_, readable := field.Value.(string)
				return !readable
			}
		}
	}
	return false
}
func selectedObservationTrace(o hook.Object, reached *hookReplayReach) hook.Object {
	workspace := asObject(objGet(o, "workspace"))
	if workspace == nil {
		markHookReplay(reached, "selected_marker", 1)
		return o
	}
	markHookReplay(reached, "selected_marker", 2)
	stop := asObject(objGet(o, "stop_input"))
	session := objGet(stop, "session_id")
	items, _ := objGet(workspace, "assignments").([]any)
	type row struct {
		at      time.Time
		id      string
		m       hook.Object
		claimed bool
	}
	var rows []row
	for _, raw := range items {
		m := asObject(raw)
		intent := asObject(objGet(m, "intent"))
		at := parseMoment(objGet(intent, "declaredAt"))
		if at == nil {
			continue
		}
		id := ""
		if value := objGet(m, "assignmentId"); evidence.Truthy(value) {
			id = evidence.Text(value)
		}
		rows = append(rows, row{*at, id, m, selectingClaimTrace(m, session, id, reached) || obstructedClaim(m, session)})
	}
	pool := rows
	has := false
	for _, r := range rows {
		has = has || r.claimed
	}
	if has {
		pool = nil
		for _, r := range rows {
			if r.claimed {
				pool = append(pool, r)
			}
		}
	}
	if len(pool) == 0 {
		markHookReplay(reached, "resolve_assignment", 1)
		return objSet(o, "marker", nil)
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].at.Equal(pool[j].at) {
			return pool[i].id < pool[j].id
		}
		return pool[i].at.Before(pool[j].at)
	})
	chosen := pool[len(pool)-1]
	markHookReplay(reached, "resolve_assignment", 2)
	o = objSet(o, "marker", chosen.m)
	return objSet(o, "assignment", objGet(chosen.m, "assignmentId"))
}
func cleanMarker(marker hook.Object) hook.Object {
	out := hook.Object{}
	claimIDs := map[string]string{}
	for _, f := range marker {
		if f.Value == nil {
			continue
		}
		if f.Key == "claims" {
			if items, ok := f.Value.([]any); ok {
				for i, raw := range items {
					claim := asObject(raw)
					if id, ok := objGet(claim, "factId").(string); ok && strings.Count(id, "/") == 1 {
						// hook_probe._claimant accepts the directory address used by its
						// synthetic fixtures. Product intent.claimant accepts only the file.
						claimIDs[id] = id + "/claim.json"
						items[i] = objSet(claim, "factId", claimIDs[id])
					}
				}
				f.Value = items
			}
		}
		out = append(out, f)
	}
	for i := range out {
		if out[i].Key != "resolution" && out[i].Key != "resolutions" {
			continue
		}
		items := []any{out[i].Value}
		if out[i].Key == "resolutions" {
			items, _ = out[i].Value.([]any)
		}
		for j, raw := range items {
			resolution := asObject(raw)
			adjudicated, list := objGet(resolution, "adjudicated").([]any)
			if !list {
				continue
			}
			for k, entryRaw := range adjudicated {
				entry := asObject(entryRaw)
				if id, ok := objGet(entry, "factId").(string); ok && claimIDs[id] != "" {
					adjudicated[k] = objSet(entry, "factId", claimIDs[id])
				}
			}
			items[j] = objSet(resolution, "adjudicated", adjudicated)
		}
		if out[i].Key == "resolution" {
			out[i].Value = items[0]
		} else {
			out[i].Value = items
		}
	}
	return out
}
func probeDecide(v any) (map[string]any, error) { return probeDecideTrace(v, nil) }
func probeDecideTrace(v any, reached *hookReplayReach) (map[string]any, error) {
	o := asObject(v)
	if o == nil {
		return nil, pythonAttribute(v, "get")
	}
	o, malformed := probeMalformed(o, reached)
	marker := cleanMarker(asObject(objGet(o, "marker")))
	if malformed == "" {
		if single := asObject(objGet(marker, "resolution")); len(single) > 0 {
			items, _ := objGet(marker, "resolutions").([]any)
			marker = objSet(marker, "resolutions", append(items, single))
		}
	}
	return probeDecision(o, marker, malformed, reached)
}
func runHookProbe(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return argparseMissing(stderr, "crw skill hook-probe", "command")
	}
	switch args[0] {
	case "decide":
		if len(args) != 2 {
			return argparseMissing(stderr, "crw skill hook-probe decide", "observation")
		}
		v, e := pythonReadJSONFile(args[1])
		if e != nil {
			return reportProbeFailure(stderr, e)
		}
		root := asObject(v)
		if root == nil {
			fmt.Fprintln(stderr, pythonAttribute(v, "get"))
			return 1
		}
		payload := objGet(root, "observation")
		if !objHas(root, "observation") {
			payload = root
		}
		got, e := probeDecide(payload)
		if e != nil {
			fmt.Fprintln(stderr, e)
			return 1
		}
		_ = emitUnicode(stdout, got)
		return 0
	case "replay":
		return replayHook(args[1:], stdout, stderr)
	case "observe":
		return observeHook(args[1:], stdout, stderr)
	default:
		return invalidChoice(stderr, "crw skill hook-probe", "command", args[0], "observe", "decide", "replay")
	}
}
func replayHook(args []string, stdout, stderr io.Writer) int {
	fixtures := defaultFixture("decisions")
	contractPath := defaultContract("hook-contract.md")
	contractLabel := contractPath
	hosts := defaultFixture("host")
	fixturesFS, contractFS, hostsFS := bundledSkillFiles, bundledSkillFiles, bundledSkillFiles
	allow := false
	reached := newHookReplayReach()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--fixtures":
			i++
			if i >= len(args) {
				return argparseValue(stderr, "crw skill hook-probe replay", "--fixtures")
			}
			fixtures = args[i]
			fixturesFS, fixtures = explicitSkillFS(fixtures)
		case "--contract":
			i++
			if i >= len(args) {
				return argparseValue(stderr, "crw skill hook-probe replay", "--contract")
			}
			contractPath = args[i]
			contractLabel = contractPath
			contractFS, contractPath = explicitSkillFS(contractPath)
		case "--host-fixtures":
			i++
			if i >= len(args) {
				return argparseValue(stderr, "crw skill hook-probe replay", "--host-fixtures")
			}
			hosts = args[i]
			hostsFS, hosts = explicitSkillFS(hosts)
		case "--allow-unreached":
			allow = true
		default:
			return invalidOption(stderr, "crw skill hook-probe replay", args[i])
		}
	}
	paths, globErr := fs.Glob(fixturesFS, filepath.ToSlash(filepath.Join(fixtures, "*.json")))
	if globErr != nil {
		fmt.Fprintf(stderr, "Probe failed: %s. Nothing was written.\n", globErr)
		return 3
	}
	// hook_probe.command_replay runs the oracle self-check first, before any
	// fixture and outside the return-site recorder.
	var oracleOut bytes.Buffer
	oracleFailures, proven, oracleErr := hookOracleSelfCheck()
	if oracleErr != nil {
		fmt.Fprintln(stderr, oracleErr)
		return 1
	}
	for _, line := range oracleFailures {
		fmt.Fprintln(&oracleOut, "FAIL oracle self-check: "+line)
	}
	oracleFailed := len(oracleFailures) > 0
	if !oracleFailed {
		fmt.Fprintf(&oracleOut, "ok   oracle self-check: %d compared keys proven load-bearing\n", proven)
	}
	oracleReport := oracleOut.String()
	fmt.Fprint(stdout, oracleReport)
	if oracleFailed {
		return 1
	}
	if len(paths) == 0 {
		fmt.Fprintln(stdout, "No fixtures carried an expectation; nothing was checked.")
		return 1
	}
	checked, failed := 0, 0
	var skipped []string
	for _, p := range paths {
		// json.loads(path.read_text()): an OSError reaches main (exit 3); a
		// UnicodeDecodeError or JSONDecodeError is uncaught (exit 1).
		v, e := pythonReadJSON(fixturesFS, p, "/"+p)
		if e != nil {
			return reportProbeFailure(stderr, e)
		}
		f := asObject(v)
		if f == nil {
			fmt.Fprintln(stderr, pythonAttribute(v, "get"))
			return 1
		}
		if value := objGet(f, "steps"); evidence.Truthy(value) {
			steps, err := hostList(orderedPlain(value))
			if err != nil {
				fmt.Fprintln(stderr, pythonNotIterable(value, false))
				return 1
			}
			if original, ok := value.([]any); ok {
				steps = original
			}
			good := true
			var report bytes.Buffer
			for i, s := range steps {
				step := asObject(s)
				if step == nil {
					fmt.Fprintln(stderr, pythonAttribute(s, "get"))
					return 1
				}
				matched, err := checkHookOneTrace(fmt.Sprintf("%s step %d", filepath.Base(p), i+1), objGet(step, "observation"), objGet(step, "expected"), &report, reached)
				if err != nil {
					fmt.Fprintln(stderr, err)
					return 1
				}
				if !matched {
					good = false
				}
			}
			checked++
			if !good {
				failed++
			}
			fmt.Fprint(stdout, report.String())
			continue
		}
		expectedValue := objGet(f, "expected")
		if evidence.Truthy(expectedValue) {
			if _, err := hostList(orderedPlain(expectedValue)); err != nil {
				fmt.Fprintln(stderr, pythonNotIterable(expectedValue, true))
				return 1
			}
		}
		expected := asObject(expectedValue)
		if !objHas(expected, "decision") && !objHas(expected, "state") && !objHas(expected, "observation") && !objHas(expected, "record") {
			skipped = append(skipped, filepath.Base(p))
			continue
		}
		checked++
		matched, err := checkHookOneTrace(filepath.Base(p), objGet(f, "observation"), expected, stdout, reached)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !matched {
			failed++
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintln(stdout, "SKIPPED without a decision, state, or observation expectation: "+strings.Join(skipped, ", "))
		return 1
	}
	if checked == 0 {
		fmt.Fprintln(stdout, "No fixtures carried an expectation; nothing was checked.")
		return 1
	}
	fmt.Fprintf(stdout, "%d/%d fixtures matched\n", checked-failed, checked)
	traceIDs, e := replayTraceIDs(contractFS, contractPath)
	if e != nil {
		if errors.Is(e, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "Probe failed: [Errno 2] No such file or directory: '%s'. Nothing was written.\n", contractLabel)
		} else {
			fmt.Fprintf(stderr, "Probe failed: %s. Nothing was written.\n", e)
		}
		return 3
	}
	missingTraces := replayMissingTraces(paths, traceIDs)
	if len(missingTraces) > 0 {
		fmt.Fprintln(stdout, "DOCUMENTED WITHOUT A FIXTURE: "+strings.Join(missingTraces, ", "))
		if allow {
			fmt.Fprintln(stdout, "WAIVED: --allow-unreached was passed, so incomplete documented-trace coverage did not fail this run.")
		} else {
			fmt.Fprintln(stdout, "The contract advertises these traces and nothing exercises them.")
			failed++
		}
	} else {
		fmt.Fprintf(stdout, "documented traces backed by a fixture: %d/%d\n", len(traceIDs), len(traceIDs))
	}
	sites, e := pythonReturnSites(bundledSkillFiles)
	if e != nil {
		fmt.Fprintf(stderr, "Probe failed: %s. Nothing was written.\n", e)
		return 3
	}
	missingReturns := make([]hookReplaySite, 0)
	for _, site := range sites {
		if !reached.reached[site.key] {
			missingReturns = append(missingReturns, site)
		}
	}
	sort.Slice(missingReturns, func(i, j int) bool {
		if missingReturns[i].function == missingReturns[j].function {
			return missingReturns[i].line < missingReturns[j].line
		}
		return missingReturns[i].function < missingReturns[j].function
	})
	fmt.Fprintf(stdout, "return-site coverage: %d/%d sites reached\n", len(sites)-len(missingReturns), len(sites))
	for _, site := range missingReturns {
		fmt.Fprintf(stdout, "  UNREACHED %s:%d  %s\n", site.function, site.line, site.source)
	}
	if len(missingReturns) > 0 {
		if allow {
			fmt.Fprintln(stdout, "WAIVED: --allow-unreached was passed, so unreached return sites did not fail this run. Fixture mismatches are never waived.")
		} else {
			fmt.Fprintln(stdout, "A return site no fixture executes is an untested decision path. Add a fixture for it, or pass --allow-unreached for a deliberate subset run.")
		}
	}
	hostChecked, hostProblems, hostErr := replayHostObservations(hostsFS, hosts, contractFS, contractPath)
	if hostErr != nil {
		return reportProbeFailure(stderr, hostErr)
	}
	if len(hostProblems) > 0 {
		for _, problem := range hostProblems {
			fmt.Fprintln(stdout, "HOST OBSERVATION: "+problem)
		}
		failed++
	} else {
		questions, _ := replayPacketQuestions(contractFS, contractPath)
		fmt.Fprintf(stdout, "host observations: %d record(s), each covering all %d packet rows on its own and agreeing with the capability record it names. A recording, not a live host run.\n", hostChecked, len(questions))
	}
	if failed > 0 {
		return 1
	}
	if len(missingReturns) > 0 && !allow {
		return 1
	}
	if len(missingTraces) > 0 && !allow {
		return 1
	}
	return 0
}
func checkHookOneTrace(label string, observation, expectedValue any, out io.Writer, reached *hookReplayReach) (bool, error) {
	return checkHookOneKeys(label, observation, expectedValue, out, reached, hookComparedKeys)
}
func checkHookOneKeys(label string, observation, expectedValue any, out io.Writer, reached *hookReplayReach, compared []string) (bool, error) {
	if !evidence.Truthy(observation) {
		observation = hook.Object{}
	}
	got, err := probeDecideTrace(observation, reached)
	if err != nil {
		return false, err
	}
	if !evidence.Truthy(expectedValue) {
		expectedValue = hook.Object{}
	}
	expected := asObject(expectedValue)
	if expected == nil {
		if _, err := hostList(orderedPlain(expectedValue)); err != nil {
			return false, pythonNotIterable(expectedValue, true)
		}
		return false, pythonAttribute(expectedValue, "get")
	}
	mismatch := map[string]any{}
	for _, f := range expected {
		if f.Key == "record" {
			want := asObject(f.Value)
			if evidence.Truthy(f.Value) && want == nil {
				return false, pythonAttribute(f.Value, "items")
			}
			record, _ := got["record"].(map[string]any)
			for _, field := range want {
				if !evidence.Equal(record[field.Key], field.Value) {
					mismatch["record."+field.Key] = []any{field.Value, record[field.Key]}
				}
			}
			continue
		}
		if slices.Contains(compared, f.Key) {
			if !evidence.Equal(got[f.Key], f.Value) {
				mismatch[f.Key] = []any{orderedPlain(f.Value), got[f.Key]}
			}
		}
	}
	if len(mismatch) > 0 {
		fmt.Fprintf(out, "FAIL %s: %s\n", label, evidence.Dumps(mismatch, false, true, true))
		return false, nil
	}
	fmt.Fprintf(out, "ok   %s: %s %s (observed %s)\n", label, got["decision"], got["state"], got["observation"])
	return true, nil
}

func probeFileError(err error, name string) string {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Sprintf("[Errno 13] Permission denied: '%s'", name)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("[Errno 2] No such file or directory: '%s'", name)
	}
	if errors.Is(err, syscall.EISDIR) {
		return fmt.Sprintf("[Errno 21] Is a directory: '%s'", name)
	}
	return err.Error()
}

// pythonOSErrorLine is the final traceback line of an uncaught OSError.
func pythonOSErrorLine(err error, name string) string {
	class := "OSError"
	switch {
	case errors.Is(err, fs.ErrPermission):
		class = "PermissionError"
	case errors.Is(err, fs.ErrNotExist):
		class = "FileNotFoundError"
	case errors.Is(err, syscall.EISDIR):
		class = "IsADirectoryError"
	}
	return class + ": " + probeFileError(err, name)
}

// probeOSError is an OSError that escapes to hook_probe.main, which prints
// "Probe failed: <exc>. Nothing was written." and exits 3.
type probeOSError struct{ detail string }

func (e *probeOSError) Error() string { return e.detail }

// pythonPath is str(pathlib.Path(p)): repeated and trailing separators and "."
// components go, ".." stays.
func pythonPath(p string) string {
	if p == "" {
		return "."
	}
	var kept []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	out := strings.Join(kept, "/")
	if strings.HasPrefix(p, "/") {
		out = "/" + out
	}
	if out == "" {
		return "."
	}
	return out
}

// pythonReadJSON is json.loads(path.read_text(encoding="utf-8")): an OSError
// (reported under label), a UnicodeDecodeError or JSONDecodeError (both
// ValueError), or the decoded value.
func pythonReadJSON(fsys fs.FS, name, label string) (any, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, &probeOSError{probeFileError(err, label)}
	}
	return pythonLoads(raw)
}

// pythonReadJSONFile is pythonReadJSON for a path the operator named.
func pythonReadJSONFile(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &probeOSError{probeFileError(err, pythonPath(path))}
	}
	return pythonLoads(raw)
}

// pythonLoads is json.loads over UTF-8 text, with Python's exception class.
func pythonLoads(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, &evidence.PythonError{Class: "UnicodeDecodeError", Detail: err.Error()}
	}
	value, err := hook.Decode(raw)
	if err != nil {
		return nil, &evidence.PythonError{Class: "json.decoder.JSONDecodeError", Detail: err.Error()}
	}
	return value, nil
}

// pythonValueDetail is str(exc) for a ValueError raised by pythonLoads.
func pythonValueDetail(err error) string {
	var failure *evidence.PythonError
	if errors.As(err, &failure) {
		return failure.Detail
	}
	return err.Error()
}

// reportProbeFailure prints an escaped exception the way hook_probe.main does.
func reportProbeFailure(stderr io.Writer, err error) int {
	var osError *probeOSError
	if errors.As(err, &osError) {
		fmt.Fprintf(stderr, "Probe failed: %s. Nothing was written.\n", osError.detail)
		return 3
	}
	fmt.Fprintln(stderr, err)
	return 1
}

func markHookReplay(reached *hookReplayReach, function string, ordinal int) {
	if reached != nil {
		reached.Reach(function, ordinal)
	}
}

func observeHook(args []string, stdout, stderr io.Writer) int {
	binary := ""
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		h, _ := os.UserHomeDir()
		codexHome = filepath.Join(h, ".codex")
	}
	sanitize := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--binary":
			i++
			if i >= len(args) {
				return argparseValue(stderr, "crw skill hook-probe observe", "--binary")
			}
			binary = args[i]
		case "--codex-home":
			i++
			if i >= len(args) {
				return argparseValue(stderr, "crw skill hook-probe observe", "--codex-home")
			}
			codexHome = args[i]
		case "--sanitize":
			sanitize = true
		default:
			return invalidOption(stderr, "crw skill hook-probe observe", args[i])
		}
	}
	if binary == "" {
		binary, _ = exec.LookPath("codex")
	}
	if binary == "" {
		fmt.Fprintln(stderr, "Codex binary not found. Pass --binary to point at it.")
		return 3
	}
	raw, e := os.ReadFile(binary)
	if e != nil {
		fmt.Fprintf(stderr, "Codex binary not found. Pass --binary to point at it.\n")
		return 3
	}
	needle := []byte("{\n  \"$schema\": \"http://json-schema.org/draft-07/schema#\"")
	schemas := map[string]map[string]any{}
	titles := []string{}
	for at := 0; ; {
		n := bytes.Index(raw[at:], needle)
		if n < 0 {
			break
		}
		start := at + n
		window := raw[start:]
		if len(window) > 1<<16 {
			window = window[:1<<16]
		}
		var schemaRaw json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(window))
		if dec.Decode(&schemaRaw) == nil {
			decoded, err := hook.Decode(schemaRaw)
			if err != nil {
				at = start + 1
				continue
			}
			value, _ := orderedPlain(decoded).(map[string]any)
			title, _ := value["title"].(string)
			if strings.Contains(title, ".command.") {
				if _, seen := schemas[title]; !seen {
					titles = append(titles, title)
				}
				schemas[title] = value
			}
		}
		at = start + 1
	}
	if len(schemas) == 0 {
		fmt.Fprintf(stderr, "No embedded hook schemas found in %s.\n", binary)
		return 3
	}
	events, err := probeCapabilityMatrix(titles, schemas)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	report := map[string]any{"binary": binary, "events": events, "registration": nil}
	if sanitize {
		report["binary"] = "<codex-binary>"
	} else {
		registration, err := hookRegistrations(codexHome, events)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		report["registration"] = registration
	}
	// print(json.dumps(report, indent=2, sort_keys=True)): non-ASCII escaped and
	// floats spelled as Python spells them (1.0, 1e+19, Infinity), which
	// encoding/json does not do.
	fmt.Fprintln(stdout, evidence.DumpsIndent(report, 2, true, true))
	return 0
}
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
