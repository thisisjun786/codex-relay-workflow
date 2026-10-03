package hook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type GuardOptions struct {
	Root, Now, Mode, DBPath string
	SocketPath, Program     string
	Clock                   func() time.Time
	NoRecord                bool
	DefaultDBPath           func() (string, error)
}

// Evaluate does not claim a Stop event. Cached adapters already own that claim.
// Its only writes are the hook's hold reservations and observations, never relay facts.
func Evaluate(ctx context.Context, stop Object, options GuardOptions) (verdict Object, err error) {
	if options.Now == "" {
		clock := options.Clock
		if clock == nil {
			clock = time.Now
		}
		options.Now = clock().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	}
	if options.Mode == "" {
		options.Mode = Observe
	}
	downgraded := options.Mode == Hold && options.NoRecord
	if downgraded {
		options.Mode = Observe
	}
	directory := ""
	fault := func(cause any) {
		verdict = faulted(stop, options.Now, options.Mode, cause)
		if !options.NoRecord && directory != "" {
			name, e := RecordObservation(ctx, directory, object(verdict.Get("record")), options.Root)
			if e == nil {
				verdict = verdict.Set("recordedAs", nullable(name))
			}
		}
	}
	defer func() {
		if p := recover(); p != nil {
			fault(p)
			err = nil
		}
		if err != nil {
			return
		}
		if downgraded {
			verdict = verdict.Set("modeDowngraded", "hold_requires_a_recorded_observation")
			record := object(verdict.Get("record")).Set("modeDowngraded", "hold_requires_a_recorded_observation")
			verdict = verdict.Set("record", record)
			verdict = verdict.Set("reason", pyjson.Text(verdict.Get("reason"))+" Hold mode was not applied: this evaluation was asked not to record, and a hold that publishes no observation cannot be released, counted against the bounds, or audited.")
		}
		if _, ok := verdict.Lookup("recordedAs"); !ok {
			verdict = verdict.Set("recordedAs", nil)
		}
	}()
	var marker Object
	unreadable := []string{}
	session, turn := stop.Get("session_id"), stop.Get("turn_id")
	if workspace := stop.Get("cwd"); pyvalue.Truthy(workspace) {
		path, ok := workspace.(string)
		if !ok {
			fault("TypeError: expected str, bytes or os.PathLike object, not " + pyvalue.TypeName(workspace))
			return verdict, nil
		}
		directory, marker, unreadable, err = delivery.SelectAssignmentContext(ctx, options.Root, path, session)
		if err != nil {
			fault(err)
			return verdict, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	o := Observation{Stop: stop, Marker: marker, Unreadable: unreadable, Now: options.Now}
	counters := Object{}
	if directory != "" {
		o.Assignment = filepath.Base(directory)
		if !delivery.ValidSegment(session) || !delivery.ValidSegment(turn) {
			o.Malformed = "stop_identity"
		}
		var readable bool
		o.Disposition, readable = delivery.ReadDispositionContext(ctx, directory, session, turn)
		if !readable {
			o.Unreadable = append(o.Unreadable, "disposition")
		}
		if o.Malformed == "" {
			o.Malformed = malformedDisposition(o.Disposition)
		}
		d, ok := evidence.Object(o.Disposition)
		registered, rok := evidence.Object(marker.Get("relationship"))
		if o.Malformed == "" && ok && d.Get("outcome") == "ready_for_review" && delivery.SameIdentity(d.Get("sessionId"), session) && delivery.SameIdentity(d.Get("turnId"), turn) && rok && delivery.Malformed(marker) == "" {
			path := options.DBPath
			if path == "" {
				path = pyjson.Text(object(marker.Get("intent")).Get("dbPath"))
			}
			o.Receipt, readable, err = LookupReceipt(ctx, path, options.DefaultDBPath, registered.Get("relationshipId"), session, turn, registered.Get("executionGeneration"), delivery.ClaimedDispatch(marker, session, o.Assignment))
			var raised *store.ManifestException
			if errors.As(err, &raised) {
				// The exception guard.deliverable_state lets out (a RecursionError) leaves
				// lookup_receipt too, and evaluate classifies it as a fault of this evaluation.
				fault(raised.StoredText())
				return verdict, nil
			}
			if err != nil {
				return nil, err
			}
			if !readable {
				label := "receipts"
				if o.Receipt.Get("evidence") == "deliverable_unverifiable" {
					label = "the receipt's artifacts"
				}
				o.Unreadable = append(o.Unreadable, label)
			}
		}
	}
	verdict = Decide(o, counters, options.Mode)
	if slicesOmission(pyjson.Text(verdict.Get("observation"))) && directory != "" {
		var corrupt, unreadableHistory string
		counters, corrupt, unreadableHistory = HoldCounters(ctx, directory, session, turn, options.Now, filepath.Dir(directory))
		if unreadableHistory != "" {
			o.Unreadable = append(o.Unreadable, unreadableHistory)
		}
		if corrupt != "" {
			counters = Object{{Key: "holdsThisTurn", Value: nil}}
		}
		verdict = Decide(o, counters, options.Mode)
	}
	held := false
	if verdict.Get("decision") == "block" && directory != "" {
		var e error
		held, e = reserveHold(ctx, directory, session, turn, options)
		if e != nil {
			fault(e)
			return verdict, nil
		}
		if !held {
			counts := append(Object{}, counters...)
			counts = counts.Set("holdsThisTurn", int64(1))
			verdict = Decide(o, counts, options.Mode)
		}
	}
	verdict = verdict.Set("assignmentId", o.Assignment)
	verdict = verdict.Set("counters", counters)
	if !options.NoRecord && directory != "" {
		name, e := RecordObservation(ctx, directory, object(verdict.Get("record")), options.Root)
		if e != nil {
			if held {
				if removeErr := os.Remove(filepath.Join(directory, "hook", pyjson.Text(session), pyjson.Text(turn), "hold.json")); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					e = errors.Join(e, removeErr)
				}
			}
			fault(e)
			return verdict, nil
		}
		verdict = verdict.Set("recordedAs", nullable(name))
	}
	return verdict, nil
}
func slicesOmission(s string) bool {
	for _, v := range omissions {
		if s == v {
			return true
		}
	}
	return false
}
func malformedDisposition(v any) string {
	if v == nil {
		return ""
	}
	o, ok := evidence.Object(v)
	if !ok {
		return "disposition"
	}
	for _, k := range []string{"sessionId", "turnId", "outcome"} {
		if v, present := o.Lookup(k); present {
			if _, ok := v.(string); !ok {
				return "disposition." + k
			}
		}
	}
	return ""
}
func faulted(stop Object, now, mode string, cause any) Object {
	detail := fmt.Sprint(cause)
	if !strings.Contains(detail, ": ") {
		detail = "RuntimeError: " + detail
	}
	record := Object{{Key: "observation", Value: "guard_faulted"}, {Key: "turnId", Value: stop.Get("turn_id")}, {Key: "sessionId", Value: stop.Get("session_id")}, {Key: "decisionState", Value: "guard_faulted"}, {Key: "held", Value: false}, {Key: "mode", Value: mode}, {Key: "fault", Value: detail}, {Key: "at", Value: now}}
	return Object{{Key: "decision", Value: "release"}, {Key: "state", Value: "guard_faulted"}, {Key: "observation", Value: "guard_faulted"}, {Key: "reason", Value: "The guard could not finish this evaluation: " + detail + ". Released and recorded; this is a defect in the guard rather than in the marker, and it is reported as one."}, {Key: "record", Value: record}, {Key: "fault", Value: detail}, {Key: "assignmentId", Value: nil}, {Key: "counters", Value: Object{}}, {Key: "hook_output", Value: Object{}}}
}
func listing(ctx context.Context, path, pattern string, dirs bool) ([]string, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	out := []string{}
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, false
		}
		if pattern != "" {
			match, _ := filepath.Match(pattern, e.Name())
			if !match {
				continue
			}
		}
		p := filepath.Join(path, e.Name())
		if dirs {
			info, err := os.Stat(p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, false
			}
			if !info.IsDir() {
				continue
			}
		}
		out = append(out, p)
	}
	return out, true
}

type heldRecord struct {
	session, turn string
	at            any
}

func heldRecords(ctx context.Context, root, session string) ([]heldRecord, string, bool) {
	records := []heldRecord{}
	sessions, ok := listing(ctx, root, "", true)
	if !ok {
		return records, "", false
	}
	for _, s := range sessions {
		if session != "" && filepath.Base(s) != session {
			continue
		}
		turns, ok := listing(ctx, s, "", true)
		if !ok {
			return records, "", false
		}
		for _, t := range turns {
			files, ok := listing(ctx, t, "hold.json", false)
			if !ok {
				return records, "", false
			}
			for _, p := range files {
				raw, err := readRegular(ctx, p, maxInputBytes)
				if err != nil {
					return records, "hook/" + filepath.Base(s), true
				}
				v, err := Decode(raw)
				o, ok := evidence.Object(v)
				if err != nil || !ok {
					return records, "hook/" + filepath.Base(s), true
				}
				records = append(records, heldRecord{filepath.Base(s), filepath.Base(t), o.Get("at")})
			}
		}
	}
	return records, "", true
}
func HoldCounters(ctx context.Context, directory string, session, turn any, now, workspaceRoot string) (Object, string, string) {
	counts := Object{{Key: "holdsThisTurn", Value: int64(0)}, {Key: "holdsThisGeneration", Value: int64(0)}, {Key: "holdsThisSessionWindow", Value: int64(0)}}
	records, bad, ok := heldRecords(ctx, filepath.Join(directory, "hook"), "")
	if !ok {
		return counts, "", "hook"
	}
	if bad != "" {
		return counts, bad, ""
	}
	var current int64
	for _, r := range records {
		if delivery.SameIdentity(r.session, session) && delivery.SameIdentity(r.turn, turn) {
			current++
		}
	}
	counts = counts.Set("holdsThisTurn", current)
	counts = counts.Set("holdsThisGeneration", int64(len(records)))
	assignments := []string{directory}
	if workspaceRoot != "" {
		assignments, ok = listing(ctx, workspaceRoot, "", true)
	}
	if !ok {
		return counts, "", "workspace"
	}
	horizon := delivery.Moment(now)
	var window int64
	for _, a := range assignments {
		records, bad, ok = heldRecords(ctx, filepath.Join(a, "hook"), pyjson.Text(session))
		if !ok {
			return counts, "", "hook"
		}
		if bad != "" {
			return counts, bad, ""
		}
		for _, r := range records {
			when := delivery.Moment(r.at)
			if horizon == nil || when == nil || when.After(horizon.Add(-time.Hour)) {
				window++
			}
		}
	}
	return counts.Set("holdsThisSessionWindow", window), "", ""
}
func reserveHold(ctx context.Context, directory string, session, turn any, options GuardOptions) (bool, error) {
	if !delivery.ValidSegment(session) || !delivery.ValidSegment(turn) {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	result, err := delivery.PublishContext(ctx, filepath.Join(directory, "hook", pyjson.Text(session), pyjson.Text(turn), "hold.json"), Object{{Key: "sessionId", Value: session}, {Key: "turnId", Value: turn}, {Key: "at", Value: options.Now}, {Key: "mode", Value: options.Mode}}, options.Root)
	return result == delivery.Published, err
}
func RecordObservation(ctx context.Context, directory string, record Object, root string) (string, error) {
	session, turn := record.Get("sessionId"), record.Get("turnId")
	if !delivery.ValidSegment(session) || !delivery.ValidSegment(turn) {
		return "", nil
	}
	relative := filepath.Join("hook", pyjson.Text(session), pyjson.Text(turn))
	folder := filepath.Join(directory, relative)
	for range 64 {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		paths, ok := listing(ctx, folder, "*.json", false)
		if !ok {
			return "", fmt.Errorf("the hook observation directory for %s/%s cannot be read, so no slot can be allocated in it", pyjson.Text(session), pyjson.Text(turn))
		}
		index := 0
		for _, p := range paths {
			stem := strings.TrimSuffix(filepath.Base(p), ".json")
			if n, err := strconv.Atoi(stem); err == nil && n >= index {
				index = n + 1
			}
		}
		name := strconv.Itoa(index)
		result, err := delivery.PublishContext(ctx, filepath.Join(folder, name+".json"), record, root)
		if err != nil {
			return "", err
		}
		if result == delivery.Published {
			return filepath.Join(relative, name), nil
		}
	}
	return "", fmt.Errorf("could not publish an observation for %s/%s after 64 attempts; every allocated slot was taken by another writer first", pyjson.Text(session), pyjson.Text(turn))
}
