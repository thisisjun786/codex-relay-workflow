package shadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
)

// ReportSchema is the format name of the shadow report.
const ReportSchema = "crw-flow-shadow/1"

// The closed set of classes a decision pair falls in.
const (
	ClassSame                = "same"
	ClassDifferentOrder      = "different_order"
	ClassExtraSpawn          = "extra_spawn"
	ClassMissingSpawn        = "missing_spawn"
	ClassDifferentTier       = "different_tier"
	ClassDifferentEscalation = "different_escalation"
)

// Classes lists the closed set in report order.
var Classes = []string{ClassSame, ClassDifferentOrder, ClassExtraSpawn, ClassMissingSpawn, ClassDifferentTier, ClassDifferentEscalation}

// Config is what a shadow comparison needs besides the trace.
type Config struct {
	// Script is the workflow W: its bytes, its display name and the directory its relative imports read from (nil: none).
	Script []byte
	Name   string
	Dir    fs.FS
	// Args are the script's args as JSON (empty: null).
	Args json.RawMessage
	// Policy is the role table the script's calls resolve against.
	Policy *engine.Policy
	Pool   int
}

// Report is the result of a shadow comparison.
type Report struct {
	Schema string         `json:"schema"`
	Trace  TraceRef       `json:"trace"`
	Script ScriptRef      `json:"script"`
	Runs   []RunReport    `json:"runs"`
	Counts map[string]int `json:"counts"`
}

// TraceRef names the trace the report was computed from.
type TraceRef struct {
	StoreDigest string `json:"storeDigest"`
}

// ScriptRef names the script that was replayed.
type ScriptRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// RunReport is the comparison for one recorded run.
type RunReport struct {
	RunID string `json:"runId"`
	// RecordedScriptDigest is the digest of the script the run recorded; it differs from the replayed script's when the
	// comparison asks what another script would have done. ArgsMatch says whether the args given are the recorded ones.
	RecordedScriptDigest string `json:"recordedScriptDigest"`
	ArgsMatch            bool   `json:"argsMatch"`
	// Backend is the backend the run's started row records (session or app-server; a run that records none is an App Server run).
	// The replay answers either from the trace.
	Backend  string         `json:"backend"`
	Replay   ReplayInfo     `json:"replay"`
	Actual   int            `json:"actual"`
	Computed int            `json:"computed"`
	Counts   map[string]int `json:"counts"`
	Items    []Item         `json:"items"`
}

// ReplayInfo says how the replayed run ended and where it was answered from.
type ReplayInfo struct {
	Status    string `json:"status"`
	ErrorKind string `json:"errorKind,omitempty"`
	Error     string `json:"error,omitempty"`
	// Replayed calls were answered from the trace; Invented calls are calls the script made that the trace has no record of
	// (answered by the deterministic fake, so what follows them is not evidence).
	Replayed int `json:"replayed"`
	Invented int `json:"invented"`
}

// Item is one classified pair: the recorded decision, the computed one, or both.
type Item struct {
	Class    string    `json:"class"`
	Kind     string    `json:"kind"`
	Node     string    `json:"node,omitempty"`
	Actual   *Decision `json:"actual,omitempty"`
	Computed *Decision `json:"computed,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Compare replays the script on every run of the trace (Select the trace first to replay one) and classifies the decisions the
// script makes against the ones the run recorded. No agent is called: a replay backend answers from the trace.
func Compare(ctx context.Context, tr *trace.Trace, cfg Config) (*Report, error) {
	if cfg.Policy == nil {
		return nil, errors.New("shadow: a role policy is required")
	}
	prog, err := script.Load(script.Source{Name: cfg.Name, Text: cfg.Script, Dir: cfg.Dir})
	if err != nil {
		return nil, fmt.Errorf("the script does not load: %w", err)
	}
	known, err := BatchSchemas(ctx)
	if err != nil {
		return nil, fmt.Errorf("the batch verdict schema cannot be read: %w", err)
	}
	rep := &Report{Schema: ReportSchema, Trace: TraceRef{StoreDigest: tr.Source.Digest}, Script: ScriptRef{Name: cfg.Name, Digest: prog.Digest}, Runs: []RunReport{}, Counts: zeroCounts()}
	for i := range tr.Runs {
		rr, err := compareRun(ctx, &tr.Runs[i], prog, cfg, known)
		if err != nil {
			return nil, err
		}
		rep.Runs = append(rep.Runs, *rr)
		for c, n := range rr.Counts {
			rep.Counts[c] += n
		}
	}
	return rep, nil
}

func zeroCounts() map[string]int {
	m := map[string]int{}
	for _, c := range Classes {
		m[c] = 0
	}
	return m
}

func argsDigest(args json.RawMessage) string {
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	sum := sha256.Sum256(args)
	return hex.EncodeToString(sum[:])
}

func compareRun(ctx context.Context, run *trace.Run, prog *script.Program, cfg Config, known map[string]bool) (*RunReport, error) {
	if len(cfg.Args) > 0 && !json.Valid(cfg.Args) {
		return nil, errors.New("shadow: args is not valid JSON")
	}
	backend := NewReplay(run.Calls)
	journal := engine.NewMemoryJournal()
	res, err := engine.Run(ctx, prog, engine.Config{
		RunID: run.RunID, Backend: backend, Policy: cfg.Policy, Journal: journal, Args: cfg.Args, Pool: cfg.Pool, OmitSeq: true,
		Now: recordedClock(run, journal),
		Workflows: func(name, scriptPath string) (*script.Program, error) {
			if cfg.Dir == nil {
				return nil, errors.New("an ad-hoc script has no workflows directory")
			}
			return loadSibling(cfg.Dir, name, scriptPath)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", run.RunID, err)
	}
	batch := backend.BatchSchemas()
	for d := range known {
		batch[d] = true
	}
	computed := Derive(journal.Events(), batch)
	actual := Actual(run, batch)
	rr := &RunReport{
		RunID: run.RunID, RecordedScriptDigest: run.ScriptDigest, ArgsMatch: run.ArgsDigest == argsDigest(cfg.Args), Backend: recordedBackend(run),
		Replay: ReplayInfo{Status: res.Status, ErrorKind: res.ErrorKind, Error: res.Error, Replayed: backend.Replayed(), Invented: backend.Invented()},
		Actual: len(actual), Computed: len(computed),
	}
	rr.Items = Classify(actual, computed)
	rr.Counts = zeroCounts()
	for _, it := range rr.Items {
		rr.Counts[it.Class]++
	}
	return rr, nil
}

// Classify pairs the decisions of the two sides and puts every pair in the closed set.
//
// A decision is paired with the decision of the other side that has its key: a spawn is (node, role family, nth of that in the
// node), or for a call without a node (label, family, nth); a flush is (node, nth flush of the node); a tier step is (node,
// nth step of the node); an escalation is (node, nth escalation of the node). A pair whose roles or pairs differ is
// different_tier (a flush and a spawn are different tiers when their roles differ); a pair of escalations with different reasons is
// different_escalation. A recorded spawn or flush the script does not make is missing_spawn, a spawn or flush only the script
// makes is extra_spawn; a tier step or an escalation present on one side only is different_tier or different_escalation. Of the
// pairs that agree in everything else, those whose place among the decisions of their node differs are different_order (the
// order of the decisions of the calls with no node is not compared: parallel calls finish in any order); the rest are same.
func Classify(actual, computed []Decision) []Item {
	type slot struct{ a, c *Decision }
	var order []string
	slots := map[string]*slot{}
	place := func(side []Decision, actualSide bool) {
		count := map[string]int{}
		for i := range side {
			d := &side[i]
			base := keyBase(d)
			count[base]++
			key := base + "#" + strconv.Itoa(count[base])
			s, ok := slots[key]
			if !ok {
				s = &slot{}
				slots[key] = s
				order = append(order, key)
			}
			if actualSide {
				s.a = d
			} else {
				s.c = d
			}
		}
	}
	place(actual, true)
	place(computed, false)
	// a stable report order: by the recorded side's order, then the computed side's for what has no recorded decision
	sort.SliceStable(order, func(i, k int) bool {
		a, b := slots[order[i]], slots[order[k]]
		switch {
		case a.a != nil && b.a != nil:
			return a.a.Order < b.a.Order
		case a.a != nil:
			return true
		case b.a != nil:
			return false
		}
		return a.c.Order < b.c.Order
	})
	// the place of each paired decision among the paired decisions of its node, on each side
	rankA, rankC := map[string]int{}, map[string]int{}
	perNode := map[string][]string{}
	for _, key := range order {
		if s := slots[key]; s.a != nil && s.c != nil && s.a.Node != "" {
			perNode[s.a.Node] = append(perNode[s.a.Node], key)
		}
	}
	for _, keys := range perNode {
		byA := append([]string(nil), keys...)
		sort.SliceStable(byA, func(i, k int) bool { return slots[byA[i]].a.Order < slots[byA[k]].a.Order })
		byC := append([]string(nil), keys...)
		sort.SliceStable(byC, func(i, k int) bool { return slots[byC[i]].c.Order < slots[byC[k]].c.Order })
		for i, key := range byA {
			rankA[key] = i
		}
		for i, key := range byC {
			rankC[key] = i
		}
	}
	items := make([]Item, 0, len(order))
	for _, key := range order {
		s := slots[key]
		it := Item{Actual: s.a, Computed: s.c}
		d := s.a
		if d == nil {
			d = s.c
		}
		it.Kind, it.Node = d.Kind, d.Node
		switch {
		case s.a == nil:
			it.Class = missingOrExtra(d.Kind, true)
			it.Detail = "the script decides " + d.Kind + " that the recorded run did not"
		case s.c == nil:
			it.Class = missingOrExtra(d.Kind, false)
			it.Detail = "the recorded run decided " + d.Kind + " that the script does not"
		default:
			it.Class, it.Detail = comparePair(s.a, s.c)
			if it.Class == ClassSame && s.a.Node != "" && rankA[key] != rankC[key] {
				it.Class = ClassDifferentOrder
				it.Detail = fmt.Sprintf("the decision is %d. of its node on the recorded run and %d. in the script", rankA[key]+1, rankC[key]+1)
			}
		}
		items = append(items, it)
	}
	return items
}

// keyBase is the key of a decision without its ordinal.
func keyBase(d *Decision) string {
	switch d.Kind {
	case KindSpawn:
		if d.Node == "" {
			return "spawn||" + d.Family + "|" + d.Label
		}
		return "spawn|" + d.Node + "|" + d.Family
	default:
		return d.Kind + "|" + d.Node
	}
}

// missingOrExtra is the class of a decision on one side only. onlyComputed is true when the script made it.
func missingOrExtra(kind string, onlyComputed bool) string {
	switch kind {
	case KindSpawn, KindFlush:
		if onlyComputed {
			return ClassExtraSpawn
		}
		return ClassMissingSpawn
	case KindTier:
		return ClassDifferentTier
	}
	return ClassDifferentEscalation
}

// comparePair judges two decisions with the same key.
func comparePair(a, c *Decision) (string, string) {
	switch a.Kind {
	case KindSpawn, KindFlush:
		if a.Role != c.Role {
			return ClassDifferentTier, fmt.Sprintf("the recorded call is %s, the script's is %s", a.Role, c.Role)
		}
		if a.Pair != c.Pair {
			return ClassDifferentTier, fmt.Sprintf("the recorded call ran on %s, the script's would on %s", a.Pair, c.Pair)
		}
	case KindTier:
		if a.Role != c.Role {
			return ClassDifferentTier, fmt.Sprintf("the recorded step goes to %s, the script's to %s", a.Role, c.Role)
		}
	case KindEscalation:
		if a.Reason != c.Reason {
			return ClassDifferentEscalation, fmt.Sprintf("the recorded escalation is %q, the script's is %q", a.Reason, c.Reason)
		}
	}
	return ClassSame, ""
}

// recordedClock is the clock of a replayed run: the nth crw.clock the replayed script reads answers with the nth time the
// recorded run read, so a decision that depends on the time is computed from the recorded run and repeats. Past the last
// recorded reading the last one stays; a recorded run that read none has the time of its last journal row, and a run with no
// rows the zero time. The wall clock is never read.
func recordedClock(run *trace.Run, journal *engine.MemoryJournal) func() time.Time {
	var readings []time.Time
	for _, e := range run.Events {
		if e.Kind != engine.KindCall || e.Op != "crw.clock" || e.Event != engine.EvCompleted {
			continue
		}
		var text string
		raw, _ := e.Data["result"].(string)
		if json.Unmarshal([]byte(raw), &text) != nil {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
			readings = append(readings, at)
		}
	}
	fallback := time.Time{}
	if len(run.Events) > 0 {
		if at, err := time.Parse(time.RFC3339Nano, run.Events[len(run.Events)-1].At); err == nil {
			fallback = at
		}
	}
	return func() time.Time {
		// the readings the script made so far: the engine's other uses of the clock (a thread's creation time) read it without
		// consuming a recorded reading
		n := 0
		for _, e := range journal.Events() {
			if e.Op == "crw.clock" && e.Event == engine.EvCompleted {
				n++
			}
		}
		switch {
		case n < len(readings):
			return readings[n]
		case len(readings) > 0:
			return readings[len(readings)-1]
		}
		return fallback
	}
}

// recordedBackend is the backend the run's started rows record.
func recordedBackend(run *trace.Run) string {
	events := make([]engine.Event, 0, len(run.Events))
	for _, e := range run.Events {
		events = append(events, e.Engine(run.RunID))
	}
	return engine.RecordedBackend(events, run.RunID)
}
