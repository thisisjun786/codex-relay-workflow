package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The judgement's vocabulary, its windows, and the status a relay read failure carries.
const (
	capacityWithin, capacityUnknown             = "within", "unknown"
	capacityMeasured, capacityUnmeasured        = "measured", "unmeasured"
	capacityHold, capacityExpand                = "hold", "expand_candidate"
	capacityReadFailureExit                     = 3
	capacityReceiptWindow, capacitySignalWindow = 2 * time.Hour, time.Hour
	// capacityStateFile is where the judgement remembers the waiting sets it has seen.
	capacityStateFile = "capacity-state.json"
)

type CapacityLimits struct {
	MinWaiting          int     `json:"min_waiting"`
	PersistMinutes      float64 `json:"persist_minutes"`
	ReceiptMaxMinutes   float64 `json:"receipt_max_minutes"`
	RealertHours        float64 `json:"realert_hours"`
	LaneMax             int     `json:"lane_max"`
	MaxParentsPerFamily int     `json:"max_parents_per_family"`
}

type CapacityReport struct {
	At       time.Time        `json:"at"`
	Limits   CapacityLimits   `json:"limits"`
	Lane     CapacityLane     `json:"lane"`
	Actions  CapacityActions  `json:"actions"`
	Child429 CapacityChild429 `json:"child_429"`
	Plans    []CapacityPlan   `json:"plans"`
}

type CapacityLane struct {
	MergesLastHour *int `json:"merges_last_hour"`
}

type CapacityActions struct {
	State    string  `json:"state"`
	Incident *string `json:"incident"`
}

type CapacityChild429 struct {
	State string `json:"state"`
	Count *int   `json:"count"`
}

type CapacityReceiptWait struct {
	MedianMinutes *float64 `json:"median_minutes"`
	Count         int      `json:"count"`
}

type CapacityPlan struct {
	Plan            string              `json:"plan"`
	Project         string              `json:"project"`
	Family          string              `json:"family"`
	Parent          string              `json:"parent"`
	ParentsInFamily int                 `json:"parents_in_family"`
	Waiting         []string            `json:"waiting"`
	WaitingMinutes  float64             `json:"waiting_minutes"`
	Held            int                 `json:"held"`
	Ceiling         int                 `json:"ceiling"`
	HostMemory      string              `json:"host_memory"`
	ReceiptWait     CapacityReceiptWait `json:"receipt_wait"`
	Verdict         string              `json:"verdict"`
	Reasons         []string            `json:"reasons"`
	Alert           bool                `json:"alert"`
}

type capacityPlanRef struct {
	Plan    string `json:"plan"`
	Project string `json:"project"`
	Parent  string `json:"parent"`
	Family  string `json:"family"`
}

// capacitySettings is the Section "capacity" document; the thresholds are pointers, so an omitted
// key keeps its default.
type capacitySettings struct {
	Plans               []capacityPlanRef `json:"plans"`
	ActionsStatusURL    string            `json:"actions_status_url"`
	UsageLog            string            `json:"usage_log"`
	ChildModels         []string          `json:"child_models"`
	MinWaiting          *int              `json:"min_waiting"`
	PersistMinutes      *float64          `json:"persist_minutes"`
	ReceiptMaxMinutes   *float64          `json:"receipt_max_minutes"`
	RealertHours        *float64          `json:"realert_hours"`
	LaneMax             *int              `json:"lane_max"`
	MaxParentsPerFamily *int              `json:"max_parents_per_family"`
}

type capacityPlanState struct {
	Since     float64  `json:"since"`
	Waiting   []string `json:"waiting"`
	Alerted   []string `json:"alerted"`
	AlertedAt float64  `json:"alerted_at"`
}

type capacityState struct {
	Plans map[string]capacityPlanState `json:"plans"`
}

func capacityPick[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

func capacityLimitsFrom(s capacitySettings) CapacityLimits {
	limits := CapacityLimits{MinWaiting: 2, PersistMinutes: 30, ReceiptMaxMinutes: 15,
		RealertHours: 3, LaneMax: 9, MaxParentsPerFamily: 3}
	limits.MinWaiting = capacityPick(s.MinWaiting, limits.MinWaiting)
	limits.PersistMinutes = capacityPick(s.PersistMinutes, limits.PersistMinutes)
	limits.ReceiptMaxMinutes = capacityPick(s.ReceiptMaxMinutes, limits.ReceiptMaxMinutes)
	limits.RealertHours = capacityPick(s.RealertHours, limits.RealertHours)
	limits.LaneMax = capacityPick(s.LaneMax, limits.LaneMax)
	limits.MaxParentsPerFamily = capacityPick(s.MaxParentsPerFamily, limits.MaxParentsPerFamily)
	return limits
}

func capacityFamilyOf(ref capacityPlanRef) string {
	if ref.Family != "" {
		return ref.Family
	}
	return ref.Project
}

// Capacity judges whether there is room to add a parent, and why not. It emits the judgement and
// its evidence and changes nothing outside its own state file; creating projects and starting
// parents is the management session's work. dry judges without writing that state, and a relay read
// failure is an error the command reports as exit 3.
func Capacity(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, error) {
	settings := capacitySettings{}
	if err := cfg.Section("capacity", &settings); err != nil {
		return CapacityReport{}, fmt.Errorf("capacity: the section: %w", err)
	}
	limits, now := capacityLimitsFrom(settings), e.Now()
	report := CapacityReport{At: now, Limits: limits, Plans: []CapacityPlan{}}

	// A signal nobody could read is unknown or unmeasured, and neither ever suppresses.
	merges, err := capacityMergeCount(ctx, cfg, now.Add(-capacitySignalWindow))
	if err != nil {
		merges = nil
	}
	report.Lane = CapacityLane{MergesLastHour: merges}
	report.Actions.State, report.Actions.Incident = capacityActionsRead(ctx, settings.ActionsStatusURL)
	report.Child429.State, report.Child429.Count = capacityChild429Count(settings.UsageLog, settings.ChildModels, now.Add(-capacitySignalWindow))

	families := map[string]map[string]struct{}{}
	for _, ref := range settings.Plans {
		family := capacityFamilyOf(ref)
		if families[family] == nil {
			families[family] = map[string]struct{}{}
		}
		if ref.Parent != "" {
			families[family][ref.Parent] = struct{}{}
		}
	}

	statePath := filepath.Join(cfg.StateDir, capacityStateFile)
	previous := capacityReadState(statePath)
	next := capacityState{Plans: map[string]capacityPlanState{}}
	// With no configured plan there is nothing to read from the relay, so a host whose relay state
	// cannot be resolved still gets an empty judgement rather than a read failure.
	stateDir := ""
	if len(settings.Plans) > 0 {
		stateDir, err = e.relayHelperState(ctx, cfg)
		if err != nil {
			return CapacityReport{}, err
		}
	}

	for _, ref := range settings.Plans {
		waiting, err := capacityWaitingFor(ctx, e, cfg, ref.Plan)
		if err != nil {
			return CapacityReport{}, err
		}
		receipt, err := capacityReceiptWaitFor(ctx, stateDir, ref.Parent, now.Add(-capacityReceiptWindow))
		if err != nil {
			return CapacityReport{}, err
		}
		hostMemory := waiting.HostMemory
		if hostMemory == "" {
			hostMemory = capacityUnmeasured
		}
		plan := CapacityPlan{Plan: ref.Plan, Project: ref.Project, Family: capacityFamilyOf(ref),
			Parent: ref.Parent, ParentsInFamily: len(families[capacityFamilyOf(ref)]), Waiting: waiting.Waiting,
			Held: waiting.Held, Ceiling: waiting.Ceiling, HostMemory: hostMemory, ReceiptWait: receipt, Reasons: []string{}}

		plan.WaitingMinutes, plan.Alert = capacityPersist(&next, previous.Plans[ref.Plan], plan, limits, now)
		plan.Verdict, plan.Reasons = capacityJudge(plan, limits, report.Lane.MergesLastHour, report.Actions.Incident, report.Child429.Count)
		if plan.Verdict != capacityExpand {
			plan.Alert = false
		} else if plan.Alert {
			entry := next.Plans[ref.Plan]
			entry.Alerted, entry.AlertedAt = append([]string(nil), plan.Waiting...), float64(now.Unix())
			next.Plans[ref.Plan] = entry
		}
		report.Plans = append(report.Plans, plan)
	}
	if !dry {
		if err := capacityWriteState(statePath, next); err != nil {
			return CapacityReport{}, err
		}
	}
	return report, nil
}

// capacityPersist records this run's waiting set and reports how long it has waited and whether the
// alert may fire again: the start is kept while the same min_waiting nodes wait, and the alert is
// suppressed while the same set was alerted inside the realert window.
func capacityPersist(next *capacityState, before capacityPlanState, plan CapacityPlan, limits CapacityLimits, now time.Time) (float64, bool) {
	same := 0
	for _, key := range plan.Waiting {
		for _, previousKey := range before.Waiting {
			if key == previousKey {
				same++
			}
		}
	}
	since := before.Since
	if same < limits.MinWaiting || since == 0 {
		since = float64(now.Unix())
	}
	// A duration rather than two epoch numbers divided by sixty, which would lose the minutes
	// to float64's precision at epoch scale.
	waitingMinutes := now.Sub(time.Unix(int64(since), 0)).Minutes()
	if waitingMinutes < 0 {
		waitingMinutes = 0
	}
	alert := !slices.Equal(before.Alerted, plan.Waiting) || before.AlertedAt == 0 ||
		now.Sub(time.Unix(int64(before.AlertedAt), 0)) >= time.Duration(limits.RealertHours*float64(time.Hour))
	// The last alert is carried forward whether or not this run may alert again, so a hold that
	// sees a different waiting set does not erase the suppression of the set that was alerted.
	entry := capacityPlanState{Since: since, Waiting: append([]string(nil), plan.Waiting...),
		Alerted: append([]string(nil), before.Alerted...), AlertedAt: before.AlertedAt}
	next.Plans[plan.Plan] = entry
	return waitingMinutes, alert
}

func capacityJudge(plan CapacityPlan, limits CapacityLimits, lane *int, incident *string, child429 *int) (string, []string) {
	reasons := []string{}
	for _, check := range []struct {
		reason string
		hit    bool
	}{
		{"not_persistent", len(plan.Waiting) < limits.MinWaiting || plan.WaitingMinutes < limits.PersistMinutes},
		{"host_memory", plan.HostMemory != capacityWithin},
		{"child_429", child429 != nil && *child429 > 0},
		{"actions_incident", incident != nil},
		{"release_lag", plan.Held < plan.Ceiling},
		{"lane_saturated", lane != nil && *lane >= limits.LaneMax},
		{"parent_busy", plan.ReceiptWait.MedianMinutes != nil && *plan.ReceiptWait.MedianMinutes >= limits.ReceiptMaxMinutes},
		{"family_parent_cap", plan.ParentsInFamily >= limits.MaxParentsPerFamily},
	} {
		if check.hit {
			reasons = append(reasons, check.reason)
		}
	}
	if len(reasons) > 0 {
		return capacityHold, reasons
	}
	return capacityExpand, []string{}
}

func capacityReadState(path string) capacityState {
	state := capacityState{Plans: map[string]capacityPlanState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	var stored capacityState
	if err := json.Unmarshal(data, &stored); err != nil || stored.Plans == nil {
		return state
	}
	return stored
}

func capacityWriteState(path string, state capacityState) error {
	if state.Plans == nil {
		state.Plans = map[string]capacityPlanState{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, capacityStateFile+".*")
	if err != nil {
		return err
	}
	name := temp.Name()
	_, writeErr := temp.Write(append(data, '\n'))
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(name, path)
	}
	if writeErr != nil {
		os.Remove(name)
		return writeErr
	}
	return nil
}

var capacityCommand = Command{Name: "capacity", Summary: "judge whether a parent can be added, and why not", Run: capacityRun}

func init() { Register(capacityCommand) }

// capacityConfig is the configuration the command judges with: the defaults, because reading the
// management session's file is a later issue's job (config.go).
var capacityConfig = func(e *Env) *Config { return coreDefaults(e) }

const capacityUsage = "usage: crw manage capacity [--text] [--dry-run]"

func capacityRun(ctx context.Context, e *Env, args []string) int {
	asText, dry := false, false
	for _, arg := range args {
		switch arg {
		case "--text":
			asText = true
		case "--dry-run":
			dry = true
		case "-h", "--help", "help":
			fmt.Fprintln(e.Stdout, capacityUsage)
			return 0
		default:
			fmt.Fprintln(e.Stderr, capacityUsage)
			fmt.Fprintf(e.Stderr, "crw manage capacity: error: unexpected argument %q\n", arg)
			return usageExit
		}
	}
	report, err := Capacity(ctx, e, capacityConfig(e), dry)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage capacity: error: %v\n", err)
		return capacityReadFailureExit
	}
	if asText {
		for _, line := range capacityLines(report) {
			fmt.Fprintln(e.Stdout, line)
		}
		return 0
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage capacity: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	return 0
}

func capacityLines(report CapacityReport) []string {
	lines := make([]string, 0, len(report.Plans))
	for _, plan := range report.Plans {
		median, merges, alert := "unmeasured", "unknown", ""
		if plan.ReceiptWait.MedianMinutes != nil {
			median = fmt.Sprintf("%.1f minutes", *plan.ReceiptWait.MedianMinutes)
		}
		if report.Lane.MergesLastHour != nil {
			merges = fmt.Sprintf("%d", *report.Lane.MergesLastHour)
		}
		if len(plan.Reasons) > 0 {
			alert = " (" + strings.Join(plan.Reasons, ",") + ")"
		}
		if plan.Verdict == capacityExpand {
			alert += fmt.Sprintf("; alert %t", plan.Alert)
		}
		lines = append(lines, fmt.Sprintf("capacity: %s parent %s family %s: %s%s; waiting %d [%s] for %.0f minutes;"+
			" slots %d/%d; host %s; receipts %s of %d; merges %s", plan.Plan, plan.Parent, plan.Family, plan.Verdict,
			alert, len(plan.Waiting), strings.Join(plan.Waiting, ","), plan.WaitingMinutes, plan.Held, plan.Ceiling,
			plan.HostMemory, median, plan.ReceiptWait.Count, merges))
	}
	return lines
}
