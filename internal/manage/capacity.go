package manage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

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

	// unusable is why the state the report was judged on could not be used: it could not be read,
	// or not be given a lineage (pumpSignalCapacityRead). It is not part of the document.
	unusable string
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
	AlertEpoch      int                 `json:"-"`
	Branches        *BranchCandidates   `json:"branches,omitempty"`
	// BranchesUnmeasured is why the plan's branches were not measured: a store that predates the DAG
	// zone holds no plan to read. The branches field is null beside it, which is how a reader tells
	// "nobody could look" from the empty list of a plan nothing can be detached from.
	BranchesUnmeasured string `json:"branches_unmeasured,omitempty"`

	// lifecycle names the plan's entry in the capacity state (capacityPlanState.Lifecycle) and
	// committedEpoch is the alert epoch that entry held when the plan was judged. proposal is set
	// when the state read held no lifecycle for the plan: lifecycle is then the one this evaluation
	// gives the entry, and its commit stores it. removed is the version of the write that removed the
	// plan's entry when the state read holds none since (capacityState.Removed). None is part of the
	// document: the pump keeps them beside an alert so a retry of the same judgement is told from a
	// later alert of another lifecycle, and a commit it lost track of from one it never made.
	lifecycle      string
	committedEpoch int
	proposal       *capacityLifecycleProposal
	removed        int
}

// capacityLifecycleProposal is a lifecycle given to a plan's entry that the state read held none
// for, whether that entry was held at all (Held is an entry written before entries carried a
// lifecycle, not Held a plan the state does not hold), and the generation of the state it was given
// on: the stamp, the lineage and the version (capacityLoadState, capacityState). A caller proposes
// it back to a later evaluation (capacityWithLifecycles), which takes it only while it holds.
type capacityLifecycleProposal struct {
	Lifecycle string `json:"lifecycle"`
	Held      bool   `json:"held"`
	Stamp     string `json:"stamp"`
	Lineage   string `json:"lineage,omitempty"`
	Version   int    `json:"version,omitempty"`
}

// holds reports whether the proposal still names plan's entry on state, read with stamp, where held
// says whether the state holds an entry for plan. It holds only for the same case (an entry still
// held without a lifecycle, or a plan still absent) on the generation it was given on, or on a later
// write of that generation's lineage that neither lost the file nor removed the plan's entry: the
// same lineage (or, for a proposal given on a file without one, the lineage that started over that
// very file), a version no older and no removal of the entry since. An entry made since carries a
// lifecycle and takes no proposal, and one made and removed since left its removal. A proposal given
// on no file names no generation, since a file made and lost again leaves nothing to tell it by, and
// never holds. What does not hold draws a fresh lifecycle: at worst the alert it carries is sent
// again under a new id, never absorbed into one already sent.
func (p capacityLifecycleProposal) holds(plan string, held bool, state capacityState, stamp string) bool {
	if p.Lifecycle == "" || p.Held != held {
		return false
	}
	switch p.Stamp {
	case "", capacityStampAbsent, capacityStampUnreadable:
		return false
	case stamp:
		return true
	}
	switch {
	case state.Lineage == "":
		return false
	case p.Lineage != "" && state.Lineage != p.Lineage:
		return false
	case p.Lineage == "" && state.Origin != p.Stamp:
		return false
	}
	return state.Version >= p.Version && state.Removed[plan] <= p.Version
}

// MarshalJSON keeps a plan's three answers about its branches apart in the document: the key is
// absent when nobody looked (a hold plan without --branches-always), null beside
// branches_unmeasured when the store holds no DAG zone to measure them from, and a list otherwise.
// A reader can then tell "nobody could look" from "nothing was there".
func (p CapacityPlan) MarshalJSON() ([]byte, error) {
	type plan CapacityPlan
	wire := struct {
		plan
		Branches           json.RawMessage `json:"branches,omitempty"`
		BranchesUnmeasured string          `json:"branches_unmeasured,omitempty"`
	}{plan: plan(p), BranchesUnmeasured: p.BranchesUnmeasured}
	switch {
	case p.BranchesUnmeasured != "":
		wire.Branches = json.RawMessage("null")
	case p.Branches != nil:
		encoded, err := json.Marshal(p.Branches)
		if err != nil {
			return nil, err
		}
		wire.Branches = encoded
	}
	return json.Marshal(wire)
}

type capacityPlanRef struct {
	Plan    string `json:"plan"`
	Project string `json:"project"`
	Parent  string `json:"parent"`
	Family  string `json:"family"`
}

// capacitySettings is the Section "capacity" document; the thresholds are pointers so an omitted key
// keeps its default.
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
	Since      float64  `json:"since"`
	Waiting    []string `json:"waiting"`
	Alerted    []string `json:"alerted"`
	AlertedAt  float64  `json:"alerted_at"`
	AlertEpoch int      `json:"alert_epoch"`
	// Lifecycle names this entry from the evaluation that created it until the plan leaves the
	// state: a save that only carries the entry forward keeps it, and an entry made anew (a plan
	// registered again, a state file that was lost or written again without the plan) takes another.
	// It is a random name drawn once, when the entry is made (capacityProposedLifecycle), and never
	// derived from the file's stamp, so no two entries share it whatever the file went through. A
	// caller that must retry the commit of a new entry under the same name (the pump, whose pending
	// alert carries it) proposes that name to the evaluation with capacityWithLifecycles.
	Lifecycle string `json:"lifecycle,omitempty"`
}

type capacityState struct {
	Plans map[string]capacityPlanState `json:"plans"`
	// Version counts the writes of the file and Revision is a random token drawn anew for every
	// write. A commit is made against the stamp (capacityLoadState) its evaluation read and is
	// refused when the file has moved since, so a late commit never overwrites a state the command
	// line or another evaluation saved in between. The version alone would not do: a file that is
	// lost and written again from nothing counts from 1 again, and only the revision tells the two
	// generations apart.
	Version  int    `json:"version"`
	Revision string `json:"revision,omitempty"`
	// Lineage names the succession of writes the file holds: a random name drawn by the write that
	// makes the file from nothing (no file, one that cannot be decoded, or one written before files
	// carried a lineage) and carried by every later write, so a file that is lost or written again
	// from nothing starts another. Origin is the stamp of the file that first write replaced. Removed
	// holds, for each plan whose entry a write of the lineage removed, the version of that write; a
	// write that makes the entry again drops it. A lifecycle proposal is told by them whether the
	// plan's entry was made or removed, or the file lost, since it was given
	// (capacityLifecycleProposal.holds).
	Lineage string         `json:"lineage,omitempty"`
	Origin  string         `json:"origin,omitempty"`
	Removed map[string]int `json:"removed,omitempty"`
}

// errCapacityStateMoved is a commit refused because the state file changed after the evaluation
// read it. The alerted state is left as the newer writer saved it and the caller evaluates again.
var errCapacityStateMoved = errors.New("capacity: the state changed since it was evaluated")

// errCapacityStateBusy is a commit refused because another writer held the state's lock for longer
// than capacityLockWait. Nothing is written and the caller's next evaluation repeats the commit.
var errCapacityStateBusy = errors.New("capacity: the state is locked by another writer")

// errCapacityStateUnsynced is a write that renamed the new state into place but whose directory
// sync failed: the state now reads as written, but a power loss can still take it back. A caller that
// reports the judgement (the command line) still reports it, since the state it wrote already
// records the alert; one that builds on the commit (the pump) treats it as not made and syncs the
// directory before it builds on the next read (capacityStateDurable).
var errCapacityStateUnsynced = errors.New("capacity: the state was written but its directory sync failed")

// capacityLockWait bounds how long a commit waits for the state's lock. A writer holds it for one
// small file write, so a longer hold is a writer that stopped, and the commit gives up rather than
// holding up its caller (the pump's round, whose other sources and deliveries come after it).
const capacityLockWait = 200 * time.Millisecond

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
// failure is an error the command reports as exit 3. A commit whose write landed but whose directory
// sync failed returns the report together with errCapacityStateUnsynced: the state already records
// the alerts the report carries, so dropping the report would leave them reported to nobody.
func Capacity(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, error) {
	for attempt := 1; ; attempt++ {
		ev, err := capacityEvaluate(ctx, e, cfg)
		if err != nil {
			return CapacityReport{}, err
		}
		if dry {
			return ev.Report, nil
		}
		// A state that moved under the evaluation (another run saved first) is judged again from the
		// newer state rather than overwritten, and so is one whose lock another writer held.
		if err := ev.Commit(); err == nil {
			return ev.Report, nil
		} else if errors.Is(err, errCapacityStateUnsynced) {
			return ev.Report, err
		} else if !(errors.Is(err, errCapacityStateMoved) || errors.Is(err, errCapacityStateBusy)) || attempt == capacityCommitAttempts {
			return CapacityReport{}, err
		}
	}
}

// capacityCommitAttempts bounds the evaluations Capacity makes while the state keeps moving.
const capacityCommitAttempts = 3

// capacityEvaluation is one judgement not yet reflected in the alerted state: the report, with the
// alerts it carries, and the state the judgement would write.
type capacityEvaluation struct {
	Report    CapacityReport
	statePath string
	base      string
	next      capacityState
}

// Commit persists the alerted state of the judgement. A caller that must make an alert durable
// elsewhere first (the pump's pending events) evaluates, records the alert, and only then commits;
// an evaluation that is never committed leaves the state as it was, so the next evaluation judges
// the same alert under the same epoch (CapacityPlan.AlertEpoch) and converges on the same event.
// The write is made under the state's lock and only if the file is still the generation the
// evaluation read; otherwise it is refused with errCapacityStateMoved and nothing is written. A lock
// held by another writer past capacityLockWait refuses it with errCapacityStateBusy.
func (ev capacityEvaluation) Commit() error {
	return capacityCommitState(ev.statePath, ev.base, ev.next)
}

// capacityEvaluate is the pure half of Capacity: it reads the signals and the previous state and
// writes nothing.
func capacityEvaluate(ctx context.Context, e *Env, cfg *Config) (capacityEvaluation, error) {
	settings := capacitySettings{}
	if err := cfg.Section("capacity", &settings); err != nil {
		return capacityEvaluation{}, fmt.Errorf("capacity: the section: %w", err)
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

	statePath := crwconfig.JoinRoot(cfg.StateDir, capacityStateFile)
	previous, stamp := capacityLoadState(statePath)
	next := capacityState{Plans: map[string]capacityPlanState{}}
	// With no configured plan there is nothing to read from the relay, so a host whose relay state
	// cannot be resolved still gets an empty judgement rather than a read failure.
	stateDir := ""
	if len(settings.Plans) > 0 {
		stateDir, err = e.relayHelperState(ctx, cfg)
		if err != nil {
			return capacityEvaluation{}, err
		}
	}
	// The store's DAG zone is read once, before the relay is asked for anything: a store that predates
	// the zone, or one an interrupted install left partial, cannot answer for a plan's branches, and a
	// partial zone makes the relay fail with a raw table error rather than a refusal. The reason then
	// travels with the plan, so the unmeasured reading survives the hold shortcut.
	zoneReason, headerMissing := "", false
	if len(settings.Plans) > 0 {
		zoneReason, headerMissing, err = capacityZoneReason(ctx, stateDir)
		if err != nil {
			return capacityEvaluation{}, err
		}
	}

	for _, ref := range settings.Plans {
		waiting, err := capacityWaitingFor(ctx, e, cfg, ref.Plan, zoneReason, headerMissing)
		if err != nil {
			return capacityEvaluation{}, err
		}
		receipt, err := capacityReceiptWaitFor(ctx, stateDir, ref.Parent, now.Add(-capacityReceiptWindow))
		if err != nil {
			return capacityEvaluation{}, err
		}
		hostMemory := waiting.HostMemory
		if hostMemory == "" {
			hostMemory = capacityUnmeasured
		}
		plan := CapacityPlan{Plan: ref.Plan, Project: ref.Project, Family: capacityFamilyOf(ref),
			Parent: ref.Parent, ParentsInFamily: len(families[capacityFamilyOf(ref)]), Waiting: waiting.Waiting,
			Held: waiting.Held, Ceiling: waiting.Ceiling, HostMemory: hostMemory, ReceiptWait: receipt, Reasons: []string{}}

		plan.WaitingMinutes, plan.Alert = capacityPersist(&next, previous.Plans[ref.Plan], plan, limits, now)
		if entry := next.Plans[ref.Plan]; entry.Lifecycle == "" {
			_, held := previous.Plans[ref.Plan]
			entry.Lifecycle = capacityProposedLifecycle(ctx, ref.Plan, held, previous, stamp)
			next.Plans[ref.Plan] = entry
			plan.proposal = &capacityLifecycleProposal{Lifecycle: entry.Lifecycle, Held: held,
				Stamp: stamp, Lineage: previous.Lineage, Version: previous.Version}
		}
		plan.lifecycle, plan.committedEpoch = next.Plans[ref.Plan].Lifecycle, previous.Plans[ref.Plan].AlertEpoch
		plan.removed = previous.Removed[ref.Plan]
		plan.Verdict, plan.Reasons = capacityJudge(plan, limits, report.Lane.MergesLastHour, report.Actions.Incident, report.Child429.Count)
		branches, unmeasured, err := branchAttach(ctx, e, cfg, stateDir, ref.Plan, plan.Verdict, zoneReason)
		if err != nil {
			return capacityEvaluation{}, err
		}
		plan.Branches, plan.BranchesUnmeasured = branches, unmeasured
		if plan.Verdict != capacityExpand {
			plan.Alert = false
		} else if plan.Alert {
			entry := next.Plans[ref.Plan]
			entry.Alerted, entry.AlertedAt = append([]string(nil), plan.Waiting...), float64(now.Unix())
			// The epoch counts this plan's alerts from the previous committed state only, so an
			// evaluation that was never committed is judged again under the same epoch.
			plan.AlertEpoch = previous.Plans[ref.Plan].AlertEpoch + 1
			entry.AlertEpoch = plan.AlertEpoch
			next.Plans[ref.Plan] = entry
		}
		report.Plans = append(report.Plans, plan)
	}
	return capacityEvaluation{Report: report, statePath: statePath, base: stamp, next: next}, nil
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
		Alerted: append([]string(nil), before.Alerted...), AlertedAt: before.AlertedAt, AlertEpoch: before.AlertEpoch,
		Lifecycle: before.Lifecycle}
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
	state, _ := capacityLoadState(path)
	return state
}

// capacityLoadState reads the state and the stamp of the file's generation: "absent" for no file,
// the version and the revision for a state, a digest of the content for a file that cannot be
// decoded (an evaluation that read it may replace it, any other may not) and for one written before
// files carried a revision (whose version does not move with its writes), and a stamp no commit
// matches for a file that cannot be read. An absent or undecodable file reads as the empty state.
func capacityLoadState(path string) (capacityState, string) {
	state := capacityState{Plans: map[string]capacityPlanState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, capacityStampAbsent
	}
	if err != nil {
		return state, capacityStampUnreadable
	}
	sum := sha256.Sum256(data)
	var stored capacityState
	if err := json.Unmarshal(data, &stored); err != nil || stored.Plans == nil {
		return state, "undecodable:" + hex.EncodeToString(sum[:])
	}
	if stored.Revision == "" {
		return stored, "content:" + hex.EncodeToString(sum[:])
	}
	return stored, fmt.Sprintf("%d/%s", stored.Version, stored.Revision)
}

const (
	capacityStampAbsent     = "absent"
	capacityStampUnreadable = "unreadable"
)

// capacityLifecyclesKey carries, on an evaluation's context, the lifecycles its caller proposes for
// plans the state does not hold yet (capacityWithLifecycles).
type capacityLifecyclesKey struct{}

// capacityWithLifecycles proposes, for each plan it names, the lifecycle an evaluation gives that
// plan's entry when the state it reads holds none. The pump proposes the name its durable pending
// record already carries, so a commit that was refused or never made is retried under the same
// lifecycle while the proposal holds (capacityLifecycleProposal.holds); a state that holds the
// plan's lifecycle keeps it and ignores the proposal.
func capacityWithLifecycles(ctx context.Context, proposed map[string]capacityLifecycleProposal) context.Context {
	return context.WithValue(ctx, capacityLifecyclesKey{}, proposed)
}

// capacityProposedLifecycle is the lifecycle an entry of plan without one takes on state, read with
// stamp: the one proposed on ctx while it holds (held tells an entry held without a lifecycle from
// an absent plan), else a random name drawn now.
func capacityProposedLifecycle(ctx context.Context, plan string, held bool, state capacityState, stamp string) string {
	if proposed, ok := ctx.Value(capacityLifecyclesKey{}).(map[string]capacityLifecycleProposal); ok {
		if p := proposed[plan]; p.holds(plan, held, state, stamp) {
			return p.Lifecycle
		}
	}
	return rand.Text()
}

// capacityCommitState writes next as the generation after base, under an exclusive lock on the
// state's lock file, and refuses when the file is no longer the generation base names or cannot be
// read. The lock is waited for at most capacityLockWait. The write carries the file's lineage on,
// or starts one over a file without (capacityState), and records the plans whose entries it removes.
func capacityCommitState(path string, base string, next capacityState) error {
	if err := os.MkdirAll(rootDir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := capacityLock(int(lock.Fd()), capacityLockWait); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	current, stamp := capacityLoadState(path)
	if stamp == capacityStampUnreadable || stamp != base {
		return errCapacityStateMoved
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	next.Version, next.Revision = current.Version+1, hex.EncodeToString(token)
	removed := map[string]int{}
	next.Lineage, next.Origin = current.Lineage, current.Origin
	if current.Lineage == "" {
		next.Lineage, next.Origin = rand.Text(), stamp
	} else {
		for name, version := range current.Removed {
			removed[name] = version
		}
	}
	for name := range current.Plans {
		if _, kept := next.Plans[name]; !kept {
			removed[name] = next.Version
		}
	}
	for name := range next.Plans {
		delete(removed, name)
	}
	next.Removed = nil
	if len(removed) > 0 {
		next.Removed = removed
	}
	return capacityWriteState(path, next)
}

// capacityStateEstablishLineage writes the capacity state at path again as it reads, when it carries
// no lineage (no file, one that cannot be decoded, or one written before files carried a lineage),
// so that it carries one: the write changes no entry, no persistence clock and no alerted set, and
// only names the generation (capacityCommitState starts the lineage). A state another writer replaced
// first is left to that writer, and a state that cannot be read is left for the caller's read to
// report. A lifecycle proposal made on a state with a lineage is told from one made on a file that
// was lost, or lost and written again with the same bytes, which a stamp of no file or of a file's
// content cannot tell (capacityLifecycleProposal.holds).
func capacityStateEstablishLineage(path string) error {
	state, stamp := capacityLoadState(path)
	if stamp == capacityStampUnreadable || state.Lineage != "" {
		return nil
	}
	if err := capacityCommitState(path, stamp, state); err != nil && !errors.Is(err, errCapacityStateMoved) {
		return fmt.Errorf("capacity: give the state a lineage: %w", err)
	}
	return nil
}

// capacityStateDurable makes the capacity state at path durable as it now reads, by syncing its
// directory: a write that renamed a state into place but whose directory sync failed can otherwise
// still be taken back by a power loss, after a caller built on it as made. No directory holds no
// state to make durable.
func capacityStateDurable(path string) error {
	if err := capacityStateIO.syncDir(rootDir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("capacity: sync the state directory: %w", err)
	}
	return nil
}

// capacityLock takes the exclusive lock on fd without blocking, trying again until wait has passed;
// a lock still held then is errCapacityStateBusy.
func capacityLock(fd int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if !time.Now().Before(deadline) {
			return errCapacityStateBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func capacityWriteState(path string, state capacityState) error {
	if state.Plans == nil {
		state.Plans = map[string]capacityPlanState{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := rootDir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := capacityStateIO.createTemp(dir, capacityStateFile+".*")
	if err != nil {
		return err
	}
	name := temp.Name()
	// The file is synced before the rename that commits it and the directory after, as the pump
	// state is: the pump records a commit as made once this returns, and a power loss that kept that
	// record beside an older capacity state would judge the committed alert again under a new epoch.
	_, writeErr := temp.Write(append(data, '\n'))
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = capacityStateIO.rename(name, path)
	}
	if writeErr != nil {
		os.Remove(name)
		return writeErr
	}
	if err := capacityStateIO.syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", errCapacityStateUnsynced, err)
	}
	return nil
}

// capacityTempFile is the part of a temporary file the state write uses.
type capacityTempFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

// capacityStateIO is the file system the state write goes through. A test replaces it to record
// the order of the writes, syncs and renames, or to make one of them fail.
var capacityStateIO = struct {
	createTemp func(dir, pattern string) (capacityTempFile, error)
	rename     func(from, to string) error
	syncDir    func(dir string) error
}{
	createTemp: func(dir, pattern string) (capacityTempFile, error) {
		file, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return file, nil
	},
	rename: os.Rename,
	syncDir: func(dir string) error {
		handle, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer handle.Close()
		return handle.Sync()
	},
}

var capacityCommand = Command{Name: "capacity", Summary: "judge whether one more 자식 can be added, and why not", Run: capacityRun}

func init() { Register(capacityCommand) }

// capacityConfig is the configuration the command judges with: the defaults, because reading the
// management session's file is a later issue's job (config.go).
var capacityConfig = func(e *Env) *Config { return coreDefaults(e) }

const capacityUsage = "usage: crw manage capacity [--text] [--dry-run] [--branches-always]"

func capacityRun(ctx context.Context, e *Env, args []string) int {
	asText, dry := false, false
	branchAlwaysSet(e, false)
	defer branchAlwaysForget(e)
	for _, arg := range args {
		switch arg {
		case "--text":
			asText = true
		case "--dry-run":
			dry = true
		case "--branches-always":
			branchAlwaysSet(e, true)
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
	if err != nil && !errors.Is(err, errCapacityStateUnsynced) {
		fmt.Fprintf(e.Stderr, "crw manage capacity: error: %v\n", err)
		return capacityReadFailureExit
	}
	if err != nil {
		// The state was renamed into place and records the alerts of this report; only its
		// directory sync failed. The report is the judgement made, so it is printed, and a power
		// loss that takes the write back makes the next run judge the alert again.
		fmt.Fprintf(e.Stderr, "crw manage capacity: warning: %v\n", err)
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
		lines = append(lines, fmt.Sprintf("capacity: %s parent (자식) %s family %s: %s%s; waiting %d [%s] for %.0f minutes;"+
			" slots %d/%d; host %s; receipts %s of %d; merges %s", plan.Plan, plan.Parent, plan.Family, plan.Verdict,
			alert, len(plan.Waiting), strings.Join(plan.Waiting, ","), plan.WaitingMinutes, plan.Held, plan.Ceiling,
			plan.HostMemory, median, plan.ReceiptWait.Count, merges))
		lines = append(lines, branchTextLines(plan)...)
	}
	return lines
}

// branchTextLines is one plan's branch reading in text: one line per candidate, in the order the
// document carries them, and one line naming why the reading is unmeasured.
func branchTextLines(plan CapacityPlan) []string {
	if plan.BranchesUnmeasured != "" {
		return []string{"branches unmeasured: " + plan.BranchesUnmeasured}
	}
	if plan.Branches == nil {
		return nil
	}
	lines := make([]string, 0, len(*plan.Branches))
	for i, candidate := range *plan.Branches {
		ids := make([]string, len(candidate.Nodes))
		for j, node := range candidate.Nodes {
			ids[j] = node.NodeID
		}
		lines = append(lines, fmt.Sprintf("capacity: %s branch %d: nodes %s ready %d/%d regions %s",
			plan.Plan, i+1, strings.Join(ids, ","), candidate.ReadyCount, len(candidate.Nodes), strings.Join(candidate.Regions, ",")))
	}
	return lines
}
