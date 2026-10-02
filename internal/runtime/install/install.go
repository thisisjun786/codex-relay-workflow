// Package install is `crw install`: the Go runtime's installer, ported by property from
// scripts/runtime_install.py (install, the automatic restore of a failed update, register-mcp
// --owner plugin, hook --owner plugin) plus the rollback, remove and status commands the Go
// runtime needs. It installs a verified release archive into
// <destination>/bin-<version>-<digest12>/bin, exercises that candidate through its own concrete
// executables, and moves the owned pointer (<destination>/current) to it under the host-wide
// promotion lock. It never runs pip, uv or python.
//
// Every read-modify-write takes the lock the Python writers take for the same file while they
// still exist (docs/port/decisions.md 33): the host record's .crw-lock (O_EXCL), the
// .promotion-lock and the staging lock (flock), the settings records' .crw-lock and the
// crw-mcp-ownership lock.
package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/exercise"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// Object is a decoded JSON object.
type Object = record.Object

// Exit statuses. Incomplete is the fourth answer runtime_install.py gave: the candidate was
// promoted (selected, and the owned pointer names it) and only the claim that records it did
// not settle. A wrapper must not read it as "nothing happened": the runtime is in service.
const (
	OK         = 0
	Refused    = 1
	Usage      = 2
	Incomplete = 3
)

// SettleSnapshotTimeout is how long settling waits for the promotion lock to take a consistent
// snapshot of what the host selects; "could not be established" is a modelled answer.
var SettleSnapshotTimeout = 5 * time.Second

// Options are what every `crw install` command reads.
type Options struct {
	Env        scope.Env
	Dest       string
	CodexHome  string
	RecordPath string
	Socket     string
	State      string
	Issue      string
	// Now and CodexVersion are seams; nil uses the clock and `codex --version`.
	Now          func() time.Time
	CodexVersion func(context.Context) *string
	// Proc is the process table remove reads (default /proc).
	Proc string
	// ScopeRegistry is the relay's production scope registry, whose claims name the state
	// directories a daemon.json may be in ("" is the relay's own, under the passwd entry's home).
	// Remove and the reclaim read it only where CODEX_SESSION_RELAY_SCOPE_DIR is unset: a relay
	// started with that set reads and claims its scope there alone, and so do they
	// (doctor.RecordedDaemons).
	ScopeRegistry string
	// StateBackup is the operator's acknowledgement that the additive DAG zone arrives (--backup-state-to): the
	// directory the whole relay state directory is copied to before the swap that brings it (zone.go). "" is
	// no acknowledgement, and that arrival then refuses naming this route.
	StateBackup string
	// CandidateSchema is a seam: nil asks the candidate binary for its declared schema.
	CandidateSchema func(ctx context.Context, binary string) Object
}

func (o Options) stamp() string {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	return now().UTC().Format("2006-01-02T15:04:05Z")
}

func (o Options) codexVersion(ctx context.Context) *string {
	if o.CodexVersion != nil {
		return o.CodexVersion(ctx)
	}
	return doctor.CodexVersion(ctx)
}

// run is one install or update.
type run struct {
	ctx         context.Context
	o           Options
	command     string
	archive     Archive
	environment string
	pointerPath string
	promoting   bool
	owned       bool
	held        *staging.Held
	steps       []any
	gate        Object
}

func (r *run) step(name string, ok bool, extra ...contract.Field) {
	r.steps = append(r.steps, append(Object{{Key: "step", Value: name}, {Key: "ok", Value: ok}}, extra...))
}

func field(key string, value any) contract.Field { return contract.Field{Key: key, Value: value} }

func strs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// recordedPointer is the pointer path the record names, or <dest>/current.
func recordedPointer(rec Object, dest string) string {
	if owned, ok := record.Get(rec, "pointer").(Object); ok {
		if path, ok := record.Get(owned, "path").(string); ok && path != "" {
			return path
		}
	}
	return pointer.Path(dest)
}

// selectsUnder is whether the record selects any component inside environment.
func selectsUnder(rec Object, environment string) bool {
	selected, _ := record.Get(rec, "selected").(Object)
	for _, f := range selected {
		if location, ok := f.Value.(string); ok && location != "" && record.Under(location, environment) {
			return true
		}
	}
	return false
}

// selectsEvery is whether the record selects every component inside environment: a promotion
// that can be finished rather than one that moved part of the host.
func selectsEvery(rec Object, environment string) bool {
	selected, _ := record.Get(rec, "selected").(Object)
	for _, c := range definition.Components {
		location, _ := record.Get(selected, c.Name).(string)
		if location == "" || !record.Under(location, environment) {
			return false
		}
	}
	return true
}

func refusedResult(command, detail, note string, extra ...contract.Field) (Object, int) {
	out := Object{field("command", command), field("applied", false), field("refused", detail)}
	out = append(out, extra...)
	return append(out, field("note", note)), Refused
}

// Install is `crw install install` and `crw install update`: one code path.
func Install(ctx context.Context, o Options, command string, source Source) (Object, int) {
	if !record.Stated(o.Issue) {
		return refusedResult(command, "--issue is written into the host record as the evidence that this command placed the owned pointer, so it has to say something", "nothing was read, nothing was built and nothing was written.")
	}
	archive, done, err := Resolve(ctx, source)
	defer done()
	if err != nil {
		var r *refusal
		detail := err.Error()
		if !errors.As(err, &r) {
			detail = "the archive could not be resolved: " + err.Error()
		}
		return refusedResult(command, detail, "nothing was unpacked and nothing under the destination or in the host record was created or changed.")
	}
	loaded := record.Load(o.RecordPath, definition.Version)
	if !loaded.Usable() {
		return refusedResult(command, loaded.Detail, "the host record is never replaced silently: it holds the only evidence of what was run here.", field("hostRecord", o.RecordPath), field("reading", loaded.Refusal()))
	}
	rec := loaded.Value.(Object)
	r := &run{ctx: ctx, o: o, command: command, archive: archive,
		environment: filepath.Join(o.Dest, "bin-"+archive.Version+"-"+archive.Digest[:12]),
		pointerPath: recordedPointer(rec, o.Dest)}
	if why := insideARuntime(o.Dest, r.pointerPath); why != "" {
		return refusedResult(command, why, "nothing was created: a candidate built there would live inside a runtime, and the pointer placed at it would loop or name a directory a later update removes.", field("destination", o.Dest), field("pointer", pointerObject(r.pointerPath)))
	}
	r.step("verify the archive against "+SumsName, true, field("archive", archive.Object()))
	result, code, next := r.take(rec)
	switch next {
	case "resume":
		return r.resume()
	case "build":
	default:
		return result, code
	}
	defer r.held.Release()
	return r.build()
}

// insideARuntime is why a destination cannot hold a candidate, or "": it is spelled through the
// owned pointer, it resolves inside the pointer's target, or it (or an ancestor, as written or
// resolved) is itself a runtime directory - a bin-* directory carrying a staging claim.
func insideARuntime(dest, pointerPath string) string {
	if record.Within(filepath.Clean(dest), filepath.Clean(pointerPath)) {
		return "the destination " + dest + " is spelled through the owned pointer " + pointerPath + ", so a candidate built there would live inside the runtime the pointer names"
	}
	resolved, err := record.Resolve(dest)
	if err != nil {
		return "the destination " + dest + " could not be resolved: " + err.Error()
	}
	if target, err := filepath.EvalSymlinks(pointerPath); err == nil && record.Within(resolved, target) {
		return "the destination " + dest + " resolves inside " + target + ", the runtime the owned pointer names"
	}
	for _, spelling := range []string{filepath.Clean(dest), resolved} {
		for p := spelling; filepath.Dir(p) != p; p = filepath.Dir(p) {
			if runtimeDirectory(filepath.Base(p)) {
				if claim := staging.ReadClaim(p); claim.State != reading.Absent {
					return "the destination " + dest + " lies inside " + p + ", a runtime directory, so a candidate built there would live inside a runtime"
				}
			}
		}
	}
	return ""
}

// take decides what to do with an existing directory of this name and, when a build follows,
// creates and claims it - deciding, creating and claiming under one .crw-lock, because between
// an exclusive mkdir and its claim an empty claimless directory reads as adoptable.
func (r *run) take(rec Object) (Object, int, string) {
	lock, err := record.LockContext(r.ctx, r.environment, 0)
	if err != nil {
		detail := "another run is deciding what to do with this directory: " + err.Error()
		if r.ctx.Err() != nil {
			detail = interrupted(err)
		}
		result, code := refusedResult(r.command, detail, "nothing was read, nothing was removed and nothing was written.", field("environment", r.environment))
		return result, code, ""
	}
	defer lock.Release()
	if _, err := os.Lstat(r.environment); err == nil {
		selected := selectsUnder(rec, r.environment)
		names := pointer.Names(r.pointerPath, r.environment)
		protected := selected || names == nil || *names
		occupied, _ := staging.DirectoryOccupied(r.environment)
		liveness, _ := staging.OwnerLiveness(r.environment)
		decision, why := staging.Decide(staging.ReadClaim(r.environment), liveness, occupied, protected, selected)
		standing := []contract.Field{field("environment", r.environment), field("archive", r.archive.Object()), field("stagingDecision", decision), field("stagingReason", why)}
		switch decision {
		case staging.Settled:
			return r.installed(standing)
		case staging.Resume:
			r.step("resume an interrupted promotion", true, field("detail", why))
			return nil, 0, "resume"
		case staging.Reclaim:
			if result, code, reclaimed := r.reclaim(standing); !reclaimed {
				return result, code, ""
			}
			r.step("reclaim abandoned staging", true, field("detail", why))
		case staging.Adopt:
			cleared := staging.ClearOwn(r.environment)
			if err := os.Remove(r.environment); err != nil {
				result, code := refusedResult(r.command, "the empty staging directory could not be taken over: "+err.Error(), "nothing else was written.", append(standing, field("residualPaths", []any{r.environment}))...)
				return result, code, ""
			}
			r.step("take over an empty staging directory", true, field("detail", why), field("clearedOwnFiles", strs(cleared)))
		default:
			result, code := refusedResult(r.command, why, "an existing runtime directory is never overwritten. Only a directory carrying a claim this command wrote, whose owner is established gone and which nothing is using, is removed. Nothing was written to the host record.", standing...)
			return result, code, ""
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		result, code := refusedResult(r.command, "whether "+r.environment+" exists could not be established: "+err.Error(), "nothing was written.")
		return result, code, ""
	}
	held, err := staging.Create(r.environment, r.o.Issue, strconv.Itoa(os.Getpid()))
	var notOwned *staging.NotOwned
	if errors.As(err, &notOwned) {
		result, code := refusedResult(r.command, "the runtime directory could not be created by this run: "+notOwned.Err.Error(), "an existing runtime directory is never overwritten, and a run only removes a directory it created itself. Nothing was written to the host record.", field("environment", r.environment))
		return result, code, ""
	}
	r.owned, r.held = true, held
	if err != nil {
		r.step("claim the runtime directory", false, field("detail", err.Error()))
		result, code := r.failed("claim the runtime directory", failure{})
		held.Release()
		return result, code, ""
	}
	r.step("claim the runtime directory", true, field("claim", staging.ClaimPath(r.environment)))
	return nil, 0, "build"
}

// installed answers a runtime whose install finished here and which the record selects: already
// installed only when, on the record read again under the promotion lock, every component
// selects it and the owned pointer names it. A split selection - one component here, another
// elsewhere - is refused, naming it, with crw install rollback as the repair: that selects every
// component of this runtime under the promotion rules, swapping nothing while the pointer already
// names it.
func (r *run) installed(standing []contract.Field) (Object, int, string) {
	refuse := func(detail, note string, extra ...contract.Field) (Object, int, string) {
		result, code := refusedResult(r.command, detail, note, append(append([]contract.Field{}, standing...), extra...)...)
		return result, code, ""
	}
	exclusive, err := record.PromoteContext(r.ctx, r.o.RecordPath, 0)
	if err != nil {
		return refuse("another run holds the promotion lock, so whether the host selects this runtime was not read on a record nobody is changing: "+err.Error(), "nothing was built and nothing was written.")
	}
	defer exclusive.Release()
	fresh := record.Load(r.o.RecordPath, definition.Version)
	if !fresh.Usable() {
		return refuse("the host record could not be read again, so whether it selects this runtime was not established: "+fresh.Detail, "nothing was built and nothing was written.", field("reading", fresh.Refusal()))
	}
	rec := fresh.Value.(Object)
	r.pointerPath = recordedPointer(rec, r.o.Dest)
	if names := pointer.Names(r.pointerPath, r.environment); names == nil || !*names {
		return refuse("this runtime is installed and selected, but the owned pointer does not name it, so the command a host reaches is not the runtime that is selected", "nothing was built and nothing was written. Run crw install rollback "+r.environment+" to point at it again, or rerun once the pointer can be read.", field("pointer", pointerObject(r.pointerPath)))
	}
	if !selectsEvery(rec, r.environment) {
		var elsewhere []string
		selected, _ := record.Get(rec, "selected").(Object)
		for _, c := range definition.Components {
			location, _ := record.Get(selected, c.Name).(string)
			if location == "" || !record.Under(location, r.environment) {
				elsewhere = append(elsewhere, c.Name+" selects "+reading.Show(record.Get(selected, c.Name)))
			}
		}
		return refuse("this runtime is installed and the owned pointer names it, but the host record selects only part of it ("+strings.Join(elsewhere, "; ")+"), so reporting it as installed would hide a split host",
			"nothing was built and nothing was written. Run crw install rollback "+r.environment+" to select every component of it under the promotion rules - the pointer already names it, so nothing is swapped - then rerun.", field("selected", record.Get(rec, "selected")))
	}
	if problems, unread := doctor.LaunchProblems(r.environment); len(problems)+len(unread) > 0 {
		repair := runtimeRepair(r.environment, r.archive.Path)
		return refuse("this runtime is selected and the owned pointer names it, but a host cannot launch it as it stands: "+strings.Join(append(problems, unread...), "; "),
			"nothing was built and nothing was written. The directory's name is fixed by this archive's digest, so it cannot be built again beside itself: restore it in place with the commands in 'repair' (the archive is the one the directory is named for, so crw comes back byte for byte), then rerun this install, which then answers alreadyInstalled.",
			field("launchable", Object{field("problems", strs(problems)), field("unread", strs(unread))}), field("repair", strs(repair)),
			field("recoveryRequires", "run the commands in 'repair', then rerun this install"))
	}
	return append(Object{field("command", r.command), field("applied", false), field("alreadyInstalled", true)}, append(standing,
		field("selected", record.Get(rec, "selected")), field("pointer", Object{field("path", r.pointerPath), field("target", r.environment)}),
		field("note", "nothing was built and nothing was written."))...), OK, ""
}

// runtimeRepair is the shell commands that make a damaged Go runtime launchable again in place:
// bin/crw extracted from archive (the archive its directory is named for) when it is missing or
// not a regular file, made executable when it is not, and each compatibility link placed again
// (a link to crw) where it does not resolve to it.
func runtimeRepair(environment, archive string) []string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	crw := filepath.Join(environment, "bin", Binary)
	var steps []string
	info, err := os.Lstat(crw)
	switch {
	case err != nil || !info.Mode().IsRegular():
		steps = append(steps, "rm -rf "+quote(crw)+" && tar -xzf "+quote(archive)+" -O "+Binary+" > "+quote(crw)+" && chmod 755 "+quote(crw))
	case unix.Access(crw, unix.X_OK) != nil:
		steps = append(steps, "chmod 755 "+quote(crw))
	}
	real, _ := record.Resolve(crw)
	for _, name := range definition.Links() {
		link := filepath.Join(environment, "bin", name)
		if resolved, err := record.Resolve(link); err != nil || resolved != real {
			steps = append(steps, "ln -sfn "+Binary+" "+quote(link))
		}
	}
	return steps
}

// reclaim removes a staging its run abandoned (a STAGING claim whose lock nobody holds, which
// the record does not select and the pointer does not name) only when nothing may still be using
// it, judged as crw install remove judges it - by identity (runtimeDir) and on the record, the
// claim and its lock read again under the promotion lock, which this run takes after the
// directory's own .crw-lock, as every path does: the record selects it, or a pointer (the
// recorded one or the default) names it or cannot be read; a live process runs out of it or
// cannot be ruled out; a relay daemon record cannot be read; a registration the host reads names
// a path inside it or cannot be read. And a runtime the record's outgoing names is never
// reclaimed: a promotion put it in service (an exit 3, or a replaced runtime whose claim never
// settled), so its claim is settled COMPLETE instead and it is kept, which crw install rollback
// returns to. Where there is no process table (darwin) nothing can be ruled out, so it is kept,
// with the recovery by hand. Anything kept is a refusal with nothing removed or built. What is
// removed goes through its tombstone (discard), so a kill leaves no claimless half. An
// interrupted run removes nothing. runtime_install.py reclaims the same staging with no such
// reading (scripts/runtime_install.py:2831-2844).
func (r *run) reclaim(standing []contract.Field) (Object, int, bool) {
	keep := func(detail, note string, extra ...contract.Field) (Object, int, bool) {
		result, code := refusedResult(r.command, detail, note, append(append([]contract.Field{}, standing...), extra...)...)
		return result, code, false
	}
	exclusive, err := record.PromoteContext(r.ctx, r.o.RecordPath, 0)
	if err != nil {
		if r.ctx.Err() != nil {
			return keep(interrupted(err), "nothing was removed, built or written.")
		}
		return keep("another run holds the promotion lock, so whether this abandoned staging is in use was not read on a record nobody is changing: "+err.Error(), "nothing was removed, built or written.")
	}
	defer exclusive.Release()
	if says, readable := claimSays(r.environment); !readable || says != staging.Staging {
		return keep("the claim here changed while this run waited for the promotion lock (it now says "+reading.Show(says)+"), so it is not read as abandoned staging", "nothing was removed, built or written; rerun to decide on what is there now.")
	}
	if liveness, detail := staging.OwnerLiveness(r.environment); liveness != staging.Dead {
		return keep("another run took this staging while this run waited: "+detail, "nothing was removed, built or written.")
	}
	fresh := record.Load(r.o.RecordPath, definition.Version)
	if !fresh.Usable() {
		return keep("the host record could not be read again, so whether this staging is in use was not established: "+fresh.Detail, "nothing was removed, built or written.", field("reading", fresh.Refusal()))
	}
	rec := fresh.Value.(Object)
	d, err := identify(r.o.Dest, filepath.Base(r.environment))
	if err != nil {
		return keep("the destination "+r.o.Dest+" could not be read: "+err.Error(), "nothing was removed, built or written.")
	}
	if u := selectedOrPointed(rec, r.o.Dest, d); u != nil {
		return keep("this staging's run is gone, but it is not abandoned: "+u.detail, "nothing was removed, built or written.", u.fields()...)
	}
	if outgoingHeld(rec, d) {
		settled := settleInService(r.o, r.environment, "the host record's outgoing selection names this runtime, so a promotion put it in service; its claim never settled, and it is kept rather than reclaimed")
		return keep("this staging's run is gone, but a promotion put it in service: the host record's outgoing selection names it, which crw install rollback returns to, so it is not abandoned staging",
			"nothing was removed or built. Its claim was settled COMPLETE ('claim'), so it now reads as a runtime whose install finished; crw install rollback "+r.environment+" puts it back in service.",
			field("outgoing", record.Get(rec, "outgoing")), field("claim", orNull(settled)))
	}
	if u, _ := runningOrRegistered(r.ctx, r.o, d); u != nil {
		if u.recovery != "" {
			// Only a recovery by hand removes it, and a staging's is not a settled runtime's.
			var fields []contract.Field
			also := ""
			if u.key != "" {
				fields = append(fields, field(u.key, u.value))
				also = ", and none of the processes named in " + u.key + " runs out of it"
			}
			return keep("this staging's run is gone, but "+u.detail, "nothing was removed, built or written.", append(fields, field("recoveryRequires",
				"this staging was left by an install that did not finish (its run holds no lock), and it was never promoted: nothing selects it, points at it or registers it. Once no crw install run and nothing it started is still running (an interrupted run's exercise ends within "+exercise.BridgeTimeout.String()+")"+also+", delete "+r.environment+" by hand and rerun the install"))...)
		}
		return keep("this staging's run is gone, but it is not abandoned: "+u.detail, "nothing was removed, built or written.", u.fields()...)
	}
	if err := r.ctx.Err(); err != nil {
		return keep(interrupted(err), "nothing was removed, built or written.")
	}
	free, residue, err := discard(r.environment, r.tombstoneRefusal)
	if errors.Is(err, errForeignTombstone) {
		return keep("the abandoned staging is not removed: "+err.Error(), "nothing was removed, built or written.", field("tombstone", filepath.Join(r.o.Dest, tombstonePrefix+filepath.Base(r.environment))))
	}
	if !free {
		return keep("the abandoned staging could not be removed: "+err.Error(), "nothing else was written.", field("residualPaths", []any{r.environment}))
	}
	if residue != "" {
		r.step("delete the abandoned staging's tombstone", false, field("detail", err.Error()), field("residualPaths", []any{residue}),
			field("recoveryRequires", "crw install remove "+residue+" finishes it"))
	}
	return nil, 0, true
}

// tombstoneRefusal is why a tombstone already at this run's directory's tombstone name may not be
// deleted before the directory is set aside there, or "" (tombstoneInUse).
func (r *run) tombstoneRefusal(tombstone string) string {
	if u, _ := tombstoneInUse(r.ctx, r.o, tombstone); u != nil {
		return u.detail
	}
	return ""
}

// settleInService writes the COMPLETE claim of a runtime that a promotion put in service when
// its claim still says STAGING and nobody holds it (why says how that is known), so that no
// later install reads it as abandoned staging and removes a directory processes may still run
// out of. nil when there is nothing to settle.
func settleInService(o Options, environment, why string) Object {
	if says, readable := claimSays(environment); !readable || says != staging.Staging {
		return nil
	}
	if liveness, _ := staging.OwnerLiveness(environment); liveness != staging.Dead {
		return nil
	}
	err := staging.WriteClaim(environment, completePayload(o.Issue))
	says, _ := claimSays(environment)
	var detail any
	if err != nil {
		detail = err.Error()
	}
	return Object{field("environment", environment), field("settled", says == staging.Complete), field("detail", detail), field("why", why)}
}

// build unpacks, records, exercises and promotes the candidate this run owns.
func (r *run) build() (Object, int) {
	if err := r.archive.Unpack(r.environment); err != nil {
		r.step("unpack the archive", false, field("detail", err.Error()))
		return r.failed("unpack the archive", failure{})
	}
	binary := filepath.Join(r.environment, "bin", Binary)
	digest, err := record.FileDigest(binary)
	if err != nil {
		r.step("read the installed binary's digest", false, field("detail", err.Error()))
		return r.failed("read the installed binary's digest", failure{})
	}
	r.step("unpack the archive", true, field("bin", filepath.Join(r.environment, "bin")), field("binaryDigest", digest))
	installs := Object{}
	var delta record.Delta
	for _, c := range definition.Components {
		entry := record.GoInstall(r.environment, c.ConsoleScript, digest, "installed by crw install from "+r.archive.Name+" into "+r.o.Dest, true)
		entry = record.Set(record.Set(entry, "version", r.archive.Version), "archiveDigest", r.archive.Digest)
		installs = append(installs, field(c.Name, entry))
		delta.Installs = append(delta.Installs, record.Named{Component: c.Name, Entry: entry})
	}
	if written, err := record.UpdateContext(r.ctx, r.o.RecordPath, definition.Version, delta); err != nil || !written.Usable() {
		return r.failed("record the install entries", failure{reading: &written, err: err})
	}
	r.step("record the install entries", true)
	measurement, points := r.measure(installs, digest)
	if len(points) > 0 {
		if appended, err := record.UpdateContext(r.ctx, r.o.RecordPath, definition.Version, record.Delta{Points: points}); err != nil || !appended.Usable() {
			return r.failed("record the measured point", failure{reading: &appended, err: err})
		}
	}
	if record.Get(measurement, "qualifyingPoint") != true {
		detail := record.Get(measurement, "refused")
		if detail == nil {
			detail = "the candidate was not exercised successfully"
		}
		r.step("exercise the candidate", false, field("detail", detail))
		return r.failed("exercise the candidate", failure{measurement: measurement})
	}
	r.step("exercise the candidate", true)
	return r.promote(installs, measurement)
}

// measure exercises both components through this install's own executables and returns the
// measurement and, only when both were exercised and every dimension observed, one point each.
func (r *run) measure(installs Object, digest string) (Object, []record.Named) {
	bin := filepath.Join(r.environment, "bin")
	relay := exercise.Relay(r.ctx, filepath.Join(bin, definition.Relay), r.o.Socket, r.o.State, r.o.Env)
	session := exercise.Session(r.ctx, filepath.Join(bin, definition.Bridge), r.o.Socket, r.o.Env)
	bridgeComponent, _ := definition.Of(definition.Bridge)
	bridge := session.Operation(bridgeComponent.IdentityTool)
	operations := []any{relay, bridge}
	qualifying := record.Get(relay, "exercised") == true && record.Get(bridge, "exercised") == true
	version := r.o.codexVersion(r.ctx)
	host, _ := os.Hostname()
	appServer := session.AppServer()
	out := Object{field("operations", operations), field("qualifyingPoint", false), field("appServer", nil), field("toolsListed", record.Get(bridge, "toolsListed"))}
	if appServer != nil {
		out = record.Set(out, "appServer", *appServer)
	}
	if !qualifying {
		return out, nil
	}
	var unobserved []string
	if version == nil {
		unobserved = append(unobserved, "codexCli")
	}
	if host == "" {
		unobserved = append(unobserved, "host")
	}
	if appServer == nil {
		unobserved = append(unobserved, "appServer")
	}
	if len(unobserved) > 0 {
		return record.Set(out, "refused", "these dimensions could not be observed: "+strings.Join(unobserved, ", ")+", so any point recorded here would carry a null dimension that can never match"), nil
	}
	var methods []string
	for _, op := range operations {
		var words []string
		for _, word := range record.Get(op.(Object), "command").([]any) {
			words = append(words, reading.Text(word))
		}
		methods = append(methods, strings.Join(words, " "))
	}
	var points []record.Named
	for _, c := range definition.Components {
		entry, _ := record.Get(installs, c.Name).(Object)
		points = append(points, record.Named{Component: c.Name, Entry: Object{
			field("install", record.Get(entry, "location")), field("installDigest", digest), field("codexCli", *version),
			field("host", host), field("appServer", *appServer), field("date", r.o.stamp()), field("measuredBy", r.o.Issue),
			field("method", strings.Join(methods, "; ")), field("exercised", true), field("archiveDigest", r.archive.Digest),
			field("digestMatchesDefinition", true),
		}})
	}
	return record.Set(out, "qualifyingPoint", true), points
}

// gateCells reads the three cells of OPS-4.4 and OPS-4.5 of the relay the record selects now (the candidate's
// own on a first install) and of the candidate binary's declared schema; swapGate (zone.go) decides on them.
func gateCells(ctx context.Context, o Options, rec Object, candidate string, candidateSchema Object) map[string]Object {
	executable := filepath.Join(candidate, "bin", definition.Relay)
	if install := selectedInstall(rec, definition.Relay); install != nil {
		if entry, ok := record.Get(install, "entryPoint").(string); ok && entry != "" {
			executable = entry
		}
	}
	if candidateSchema == nil {
		binary := filepath.Join(candidate, "bin", Binary)
		if o.CandidateSchema != nil {
			candidateSchema = o.CandidateSchema(ctx, binary)
		} else {
			candidateSchema = swapgate.CandidateSchema(ctx, binary)
		}
	}
	return map[string]Object{
		"daemon":      swapgate.DaemonCell(scope.Relay(ctx, []string{"service", "status"}, executable, o.Socket, o.State, o.Env, false, 0)),
		"inFlight":    swapgate.InflightCell(scope.Relay(ctx, []string{"doctor"}, executable, o.Socket, o.State, o.Env, false, 0), swapgate.StorePresence(o.State, o.Socket)),
		"storeSchema": swapgate.SchemaCell(swapgate.StoreSchema(ctx, o.State, o.Socket), candidateSchema),
	}
}

// selectedInstall is the install entry the record's selection names for component name.
func selectedInstall(rec Object, name string) Object {
	selected, _ := record.Get(rec, "selected").(Object)
	location, _ := record.Get(selected, name).(string)
	if location == "" {
		return nil
	}
	components, _ := record.Get(rec, "components").(Object)
	component, _ := record.Get(components, name).(Object)
	installs, _ := record.Get(component, "installs").([]any)
	for _, raw := range installs {
		if install, ok := raw.(Object); ok && record.Get(install, "location") == location {
			return install
		}
	}
	return nil
}

// outgoingOf is what is selected when a promotion starts, and whether its bytes are still there:
// the rollback baseline the promotion records.
func outgoingOf(selected Object) Object {
	out := Object{}
	for _, c := range definition.Components {
		location, _ := record.Get(selected, c.Name).(string)
		if location == "" {
			out = append(out, field(c.Name, Object{field("selected", nil)}))
			continue
		}
		info, err := os.Stat(location)
		present := err == nil && info.IsDir()
		var digest any
		if present {
			if value, err := record.FileDigest(filepath.Join(location, Binary)); err == nil && doctor.RuntimeKind(filepath.Dir(location)) == doctor.KindGoRuntime {
				digest = value
			}
		}
		out = append(out, field(c.Name, Object{field("selected", location), field("present", present), field("digest", digest)}))
	}
	return out
}

func copyObject(o Object) Object {
	if o == nil {
		return nil
	}
	return append(Object{}, o...)
}

func pointerObject(path string) Object {
	read := pointer.Read(path)
	var target any
	if read.Target != "" {
		target = read.Target
	}
	return Object{field("path", path), field("state", read.State), field("target", target), field("detail", read.Detail)}
}

// promote is the one critical section: the lock opens first, the record is read inside it, and
// the gate, the rollback baseline, the pointer reading, the ownership and second-owner checks
// and the settings transition are all decided on that reading. The selection is committed
// before the pointer moves, and the pointer is read back rather than trusted.
func (r *run) promote(installs, measurement Object) (Object, int) {
	r.promoting = true
	exclusive, err := record.PromoteContext(r.ctx, r.o.RecordPath, 0)
	if err != nil {
		r.step("take the promotion lock", false, field("detail", err.Error()))
		return r.failed("take the promotion lock", failure{err: r.ctx.Err()})
	}
	locked := true
	defer func() {
		if locked {
			exclusive.Release()
		}
	}()
	fresh := record.Load(r.o.RecordPath, definition.Version)
	if !fresh.Usable() {
		return r.failed("read the host record for promotion", failure{reading: &fresh})
	}
	rec := fresh.Value.(Object)
	ownedBefore, _ := record.Get(rec, "pointer").(Object)
	r.pointerPath = recordedPointer(rec, r.o.Dest)
	previous, _ := record.Get(rec, "selected").(Object)
	previous = copyObject(previous)
	outgoingBefore, _ := record.Get(rec, "outgoing").(Object)

	r.gate = swapGate(r.ctx, r.o, rec, r.environment, nil)
	if record.Get(r.gate, "verdict") != swapgate.Allowed {
		r.step("read whether it is safe to swap", false, field("verdict", record.Get(r.gate, "verdict")), field("blockedBy", record.Get(r.gate, "blockedBy")), field("unreadable", record.Get(r.gate, "unreadable")))
		return r.failed("read whether it is safe to swap", failure{})
	}
	r.step("read whether it is safe to swap", true, field("detail", swapgate.Allowed))

	before := pointer.Read(r.pointerPath)
	if !pointer.Usable(before.State) {
		r.step("read the owned pointer", false, field("detail", before.Detail))
		return r.failed("read the owned pointer", failure{})
	}
	if before.State == pointer.Link && !record.PlacementRecorded(ownedBefore) {
		r.step("establish the pointer is this command's", false, field("detail", "a symbolic link is already at "+r.pointerPath+" and this host record has never recorded placing one there"))
		return r.failed("establish the pointer is this command's", failure{})
	}
	leaving := inService(rec, r.pointerPath)
	owners, conflict := secondOwners(r.o.CodexHome, r.pointerPath)
	if conflict != "" {
		r.step("refuse a second owner", false, field("detail", conflict))
		return r.failed("refuse a second owner", failure{owners: owners})
	}
	r.step("refuse a second owner", true, field("detail", record.Get(owners, "detail")))
	if err := r.ctx.Err(); err != nil {
		r.step("carry the Stop settings to this runtime", false, field("detail", interrupted(err)))
		return r.failed("carry the Stop settings to this runtime", failure{err: err})
	}
	transition := checkSettings(r.o.CodexHome, r.pointerPath)
	if transition.refused != "" {
		r.step("carry the Stop settings to this runtime", false, field("detail", transition.refused))
		return r.failed("carry the Stop settings to this runtime", failure{settings: transition.report})
	}
	r.step("carry the Stop settings to this runtime", true, field("detail", record.Get(transition.report, "detail")))

	var selection []contract.Field
	for _, f := range installs {
		selection = append(selection, field(f.Key, record.Get(f.Value.(Object), "location")))
	}
	outgoing := outgoingOf(leftSelection(rec, r.pointerPath, previous))
	if err := r.ctx.Err(); err != nil {
		r.step("commit the selection", false, field("detail", interrupted(err)))
		return r.failed("commit the selection", failure{err: err, settings: transition.report})
	}
	s := swap(r.ctx, r.o, r.pointerPath, r.environment, record.Delta{
		Select:   selection,
		Pointer:  Object{field("path", r.pointerPath), field("recordedAt", r.o.stamp()), field("recordedBy", r.o.Issue)},
		Outgoing: &record.Outgoing{Value: outgoing},
	}, true, before, ownedBefore, previous, outgoingBefore)
	if s.commitFailed() {
		r.step("commit the selection", false, field("detail", commitDetail(s.committed, s.commitErr)))
		return r.failed("commit the selection", failure{reading: &s.committed, err: s.commitErr, settings: transition.report})
	}
	if !s.landed {
		r.step("replace the owned pointer", false, field("detail", s.placement("this runtime")))
		r.step("put the pointer back", record.Get(s.pointerRestored, "verified") == true, field("detail", record.Get(s.pointerRestored, "detail")))
		return r.failed("replace the owned pointer", failure{restored: s.restored, pointerRestored: s.pointerRestored, settings: transition.report})
	}
	var previousTarget any
	if before.Target != "" {
		previousTarget = before.Target
	}
	r.step("replace the owned pointer", true, field("previousTarget", previousTarget), field("target", r.environment))
	var left Object
	if leaving != "" && leaving != r.environment {
		left = settleInService(r.o, leaving, "the runtime this promotion replaced was selected and named by the pointer, so it was in service, and its claim never settled; it is kept, and now reads as a runtime whose install finished")
	}
	exclusive.Release()
	locked = false

	settled := settleClaim(r.o, r.environment, r.o.Issue)
	current := s.committed.Value.(Object)
	code := OK
	if record.Get(settled, "settled") != true {
		code = Incomplete
	}
	return Object{
		field("command", r.command), field("applied", true), field("environment", r.environment), field("archive", r.archive.Object()),
		field("hostRecord", r.o.RecordPath), field("steps", r.steps), field("installs", installs), field("measurement", measurement),
		field("promoted", true), field("inService", record.Get(settled, "inService")), field("claimSettled", record.Get(settled, "settled")),
		field("claim", settled), field("recoveryRequires", record.Get(settled, "recoveryRequires")), field("swapGate", r.gate),
		field("pointer", Object{field("path", r.pointerPath), field("target", r.environment), field("previousTarget", previousTarget),
			field("meaning", "the registered commands reach a runtime through this path. It is a way to reach one and never an identity: a process already started goes on running the runtime it started in.")}),
		field("secondOwner", owners), field("settings", transition.report), field("leftClaim", orNull(left)),
		field("selected", record.Get(current, "selected")), field("previousSelection", previous), field("outgoing", outgoing),
		field("note", "the pointer moved only after both components were exercised through this runtime's own executables and a point was recorded (OPS-2.4). The previous runtime is kept and recorded as outgoing, which crw install rollback returns to. Nothing here removes, moves or recreates the store."),
	}, code
}

// resume finishes a promotion a previous run committed and did not live to complete: the record
// selects this runtime and its claim never settled. Nothing is rebuilt or removed.
func (r *run) resume() (Object, int) {
	exclusive, err := record.PromoteContext(r.ctx, r.o.RecordPath, 0)
	if err != nil {
		detail := "another run holds the promotion lock: " + err.Error()
		if r.ctx.Err() != nil {
			detail = interrupted(err)
		}
		return refusedResult(r.command, detail, "nothing was written.", field("environment", r.environment))
	}
	locked := true
	defer func() {
		if locked {
			exclusive.Release()
		}
	}()
	fresh := record.Load(r.o.RecordPath, definition.Version)
	if !fresh.OK() {
		return refusedResult(r.command, "the host record could not be read to finish the promotion: "+fresh.Detail, "nothing was written.", field("environment", r.environment))
	}
	rec := fresh.Value.(Object)
	if !selectsEvery(rec, r.environment) {
		return refusedResult(r.command, "the host record selects part of this runtime and part of another, so there is no single promotion here to finish", "nothing was written; read the host record and decide which runtime this host is meant to be on.", field("environment", r.environment), field("selected", record.Get(rec, "selected")))
	}
	ownedBefore, _ := record.Get(rec, "pointer").(Object)
	r.pointerPath = recordedPointer(rec, r.o.Dest)
	before := pointer.Read(r.pointerPath)
	if !pointer.Usable(before.State) || (before.State == pointer.Link && !record.PlacementRecorded(ownedBefore)) {
		return refusedResult(r.command, "the owned pointer cannot be replaced: "+before.Detail, "nothing was written.", field("environment", r.environment), field("pointer", pointerObject(r.pointerPath)))
	}
	if before.State == pointer.Link {
		if target, err := pointer.TargetOf(r.pointerPath, before.Target); err != nil || !targetRecorded(rec, target) {
			return refusedResult(r.command, "the owned pointer names "+before.Target+", which this host record does not account for, so it was repointed by something other than this command and the interrupted promotion is not this run's to finish", "nothing was written.", field("environment", r.environment), field("pointer", pointerObject(r.pointerPath)))
		}
	}
	names := pointer.Names(r.pointerPath, r.environment)
	if names == nil || !*names {
		// Moving the pointer replaces the runtime a host reaches, so OPS-4.4 is asked here too.
		r.gate = swapGate(r.ctx, r.o, rec, r.environment, nil)
		if record.Get(r.gate, "verdict") != swapgate.Allowed {
			return refusedResult(r.command, "finishing this promotion moves the owned pointer, and the swap gate answered "+scopeStr(record.Get(r.gate, "verdict")), "nothing was written; the selection stays as the interrupted run committed it, and a rerun finishes it once the gate allows.", field("environment", r.environment), field("swapGate", r.gate))
		}
	}
	// One owner per surface, read now under the promotion lock: the registrations may have changed
	// since the interrupted run judged them, and finishing its promotion moves the pointer they
	// start through as a promotion does.
	owners, conflict := secondOwners(r.o.CodexHome, r.pointerPath)
	if conflict != "" {
		return refusedResult(r.command, conflict, "nothing was written; the selection stays as the interrupted run committed it, and a rerun finishes it once one owner registers each surface.", field("environment", r.environment), field("secondOwner", owners))
	}
	if err := r.ctx.Err(); err != nil {
		return refusedResult(r.command, interrupted(err), "nothing was written.", field("environment", r.environment))
	}
	transition := checkSettings(r.o.CodexHome, r.pointerPath)
	if transition.refused != "" {
		return refusedResult(r.command, transition.refused, "nothing was written.", field("environment", r.environment), field("settings", transition.report))
	}
	if names == nil || !*names {
		// The placement is recorded before the link moves, and put back with it when the move does
		// not land; the selection stays as the interrupted run committed it.
		s := swap(r.ctx, r.o, r.pointerPath, r.environment, record.Delta{Pointer: Object{field("path", r.pointerPath), field("recordedAt", r.o.stamp()), field("recordedBy", r.o.Issue)}},
			true, before, ownedBefore, nil, nil)
		if s.commitFailed() {
			return refusedResult(r.command, "the pointer ownership could not be recorded: "+commitDetail(s.committed, s.commitErr), "the pointer was not moved.", field("environment", r.environment), field("settings", transition.report))
		}
		if !s.landed {
			return refusedResult(r.command, s.placement("this runtime"), "the pointer and its ownership were put back; the selection stays as the interrupted run committed it.", field("environment", r.environment), field("pointerRestored", s.pointerRestored), field("settings", transition.report))
		}
	}
	r.step("replace the owned pointer", true, field("target", r.environment))
	exclusive.Release()
	locked = false
	settled := settleClaim(r.o, r.environment, r.o.Issue)
	code := OK
	if record.Get(settled, "settled") != true {
		code = Incomplete
	}
	return Object{
		field("command", r.command), field("applied", true), field("resumed", true), field("promoted", true), field("environment", r.environment),
		field("steps", r.steps), field("inService", record.Get(settled, "inService")), field("claimSettled", record.Get(settled, "settled")),
		field("claim", settled), field("recoveryRequires", record.Get(settled, "recoveryRequires")), field("settings", transition.report), field("swapGate", r.gate), field("secondOwner", owners),
		field("note", "a previous run committed this runtime as selected and did not live to move the pointer. Nothing was rebuilt and nothing was removed: the missing half of that promotion was written."),
	}, code
}

// The promotion's two writes that come after the settings transition - committing the
// selection and moving the pointer - are variables only so that tests can make them fail and
// prove that everything before them is put back.
var (
	commitSelection = record.UpdateContext
	placePointer    = pointer.Place
)

// commitDetail is why a commit did not land.
func commitDetail(written reading.Reading, err error) string {
	if err != nil {
		return err.Error()
	}
	return "the host record could not be read to write the selection: " + written.Detail
}

// failure is what a failed step carries into the release report.
type failure struct {
	reading         *reading.Reading
	err             error
	measurement     Object
	restored        Object
	pointerRestored Object
	settings        Object
	owners          Object
}

// failed releases the directory this run created, when the record does not select it and the
// pointer does not name it, and reports whether the destination is retriable. The selection
// and the pointer are left as found (or as a restore put them back); the store is untouched.
func (r *run) failed(step string, cause failure) (Object, int) {
	no := false
	reached := &no
	if r.promoting {
		reached = pointer.Names(r.pointerPath, r.environment)
	}
	released, decision := record.ReleaseCandidate(r.o.RecordPath, definition.Version, r.environment, reached)
	keeping := !strings.HasPrefix(decision, "dropped")
	removed := false
	var cleanupError any
	var tombstone string
	if r.owned && !keeping {
		// Through its tombstone, so a kill part-way leaves no claimless half under the name.
		var err error
		if _, tombstone, err = discard(r.environment, r.tombstoneRefusal); errors.Is(err, errForeignTombstone) {
			// The tombstone's name is somebody else's directory: this run's own candidate is deleted
			// where it stands instead.
			err = os.RemoveAll(r.environment)
		}
		if err != nil {
			cleanupError = err.Error()
		}
		_, err = os.Lstat(r.environment)
		removed = errors.Is(err, os.ErrNotExist)
	}
	retriable := !r.owned || removed
	var residual []any
	if r.owned && !removed {
		residual = append(residual, r.environment)
	}
	if tombstone != "" {
		residual = append(residual, tombstone)
	}
	if path, ok := record.Get(cause.pointerRestored, "residualPointer").(string); ok && path != "" {
		residual = append(residual, path)
	}
	if residual == nil {
		residual = []any{}
	}
	var recovery []string
	if !retriable {
		if keeping {
			recovery = append(recovery, "this runtime is selected or the pointer may name it, so it was kept deliberately and this archive cannot be installed again until the selection moves")
		} else {
			recovery = append(recovery, "remove "+r.environment+" by hand (crw install remove refuses nothing it would not); this run created it and could not remove it")
		}
	}
	if settle, ok := record.Get(cause.pointerRestored, "settleOwnership").(string); ok && settle != "" {
		recovery = append(recovery, settle)
	}
	var recoveryValue any
	if len(recovery) > 0 {
		recoveryValue = strings.Join(recovery, "; and ")
	}
	refused := "a step failed; whatever runtime was selected remains selected"
	var readingValue any
	switch {
	case cause.reading != nil && !cause.reading.Usable():
		refused, readingValue = cause.reading.Detail, cause.reading.Refusal()
	case cause.err != nil:
		refused = cause.err.Error()
	}
	var selected any
	if released.Usable() {
		selected = record.Get(released.Value.(Object), "selected")
	}
	var removedCandidate any
	if removed {
		removedCandidate = r.environment
	}
	var pointerValue any
	if r.promoting {
		pointerValue = Object{field("path", r.pointerPath), field("namesThisEnvironment", boolOrNil(reached)), field("restored", cause.restored), field("pointerRestored", cause.pointerRestored),
			field("meaning", "the pointer was not moved by this run unless 'restored' says so. Whatever a host reached before this run, it still reaches")}
	}
	out := Object{
		field("command", r.command), field("applied", false), field("steps", r.steps), field("environment", r.environment), field("archive", r.archive.Object()),
		field("selected", selected), field("hostRecordState", released.State), field("candidate", decision), field("removedCandidate", removedCandidate),
		field("cleanupError", cleanupError), field("retriable", retriable), field("failedStep", step), field("pointer", pointerValue),
		field("swapGate", r.gate), field("residualPaths", residual), field("recoveryRequires", recoveryValue), field("refused", refused), field("reading", readingValue),
	}
	if cause.measurement != nil {
		out = append(out, field("measurement", cause.measurement))
	}
	if cause.settings != nil {
		out = append(out, field("settings", cause.settings))
	}
	if cause.owners != nil {
		out = append(out, field("secondOwner", cause.owners))
	}
	return append(out, field("note", "the selection is left as found, because another run's promotion is not this run's to undo, and a candidate that is selected or whose record cannot be read is kept rather than deleted. The store is untouched.")), Refused
}

func boolOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// restorePointer puts the pointer back to what this run found - absent, or the previous link -
// and the ownership entry with it, compare-and-replace on the path this run wrote.
func restorePointer(o Options, path string, before pointer.Answer, environment string, ownedBefore Object) Object {
	var restoredTo any
	verified, settled := false, false
	detail := "this run placed no pointer, so there is nothing to put back"
	var residual any
	var wanted Object
	switch {
	case before.State == pointer.NoPointer:
		removed, why := pointer.Remove(path, environment)
		detail, verified, settled = why, removed, removed
		if removed {
			restoredTo = "absent"
		} else {
			residual = path
		}
		if ownedBefore != nil {
			wanted = record.WithoutPlacement(ownedBefore)
		}
	case before.State == pointer.Link && before.Target != "":
		settled = true
		wanted = ownedBefore
		if err := pointer.Place(path, before.Target); err != nil {
			residual, detail = path, "the previous target could not be put back: "+err.Error()
			break
		}
		names := pointer.Names(path, before.Target)
		verified = names != nil && *names
		if verified {
			restoredTo, detail = before.Target, "the previous target was put back and read back"
		} else {
			residual, detail = path, "the previous target could not be read back after restoring it"
		}
	}
	var ownership any
	var settle any
	if settled {
		delta := record.Delta{DropPointer: &path}
		if wanted != nil {
			delta = record.Delta{RestorePointer: &record.Restore{Wrote: path, Found: wanted}}
		}
		if _, err := record.Update(o.RecordPath, definition.Version, delta); err != nil {
			verified = false
			ownership = "unreadable"
			detail += ", but writing the ownership record failed: " + err.Error()
			settle = "settle the host record's pointer ownership for " + path + ": the link and the record may disagree about who placed it"
		} else if wanted == nil {
			ownership = "dropped"
		} else if record.PlacementRecorded(wanted) {
			ownership = "restored"
		} else {
			ownership = "withdrawn"
		}
	}
	return Object{field("restoredTo", restoredTo), field("verified", verified), field("residualPointer", residual), field("settleOwnership", settle), field("ownership", ownership), field("detail", detail)}
}

// restoreSelection puts back what this run's commit replaced, only where the record still holds
// what this run wrote: another run's later promotion is not this run's to undo.
func restoreSelection(o Options, previous Object, wrote []contract.Field, outgoingBefore Object) Object {
	current := record.Load(o.RecordPath, definition.Version)
	if !current.Usable() {
		return Object{field("selection", nil), field("restored", []any{}), field("detail", "the host record could not be read, so the previous selection could not be put back: "+current.Detail)}
	}
	selected, _ := record.Get(current.Value.(Object), "selected").(Object)
	var back, gone []contract.Field
	var movedOn []any
	for _, f := range wrote {
		if record.Get(selected, f.Key) != f.Value {
			movedOn = append(movedOn, f.Key)
			continue
		}
		if location, ok := record.Get(previous, f.Key).(string); ok && location != "" {
			back = append(back, field(f.Key, location))
		} else {
			gone = append(gone, f)
		}
	}
	delta := record.Delta{Select: back, Deselect: gone}
	if len(movedOn) == 0 {
		delta.Outgoing = &record.Outgoing{Value: outgoingBefore}
	}
	written, err := record.Update(o.RecordPath, definition.Version, delta)
	if err != nil || !written.Usable() {
		return Object{field("selection", nil), field("restored", []any{}), field("detail", "the selection could not be put back")}
	}
	var restored []any
	for _, f := range back {
		restored = append(restored, f.Key)
	}
	for _, f := range gone {
		restored = append(restored, f.Key)
	}
	if movedOn == nil {
		movedOn = []any{}
	}
	if restored == nil {
		restored = []any{}
	}
	return Object{field("selection", record.Get(written.Value.(Object), "selected")), field("restored", restored), field("movedOnByAnotherRun", movedOn),
		field("detail", "the selection this run moved was put back to what it was, including back to nothing where nothing was selected before it")}
}

// settleClaim writes the COMPLETE claim last and answers whether the RECORD of the replacement
// landed; the replacement itself already has. What the next run would do is derived from what
// the record says now, read under the promotion lock with a short timeout.
func settleClaim(o Options, environment, issue string) Object {
	path := staging.ClaimPath(environment)
	err := staging.WriteClaim(environment, completePayload(issue))
	var busy *record.Busy
	contended := errors.As(err, &busy)
	left := staging.ReadClaim(environment)
	says := ""
	if value, ok := left.Value.(Object); ok && left.OK() {
		says, _ = record.Get(value, "state").(string)
	}
	settled := says == staging.Complete
	var state, selects, names, protected any
	var snapshotDetail any
	finishable := false
	if exclusive, lockErr := record.Promote(o.RecordPath, SettleSnapshotTimeout); lockErr != nil {
		snapshotDetail = "a consistent snapshot of what this host selects could not be taken: " + lockErr.Error()
	} else {
		current := record.Load(o.RecordPath, definition.Version)
		state = current.State
		if current.OK() {
			rec := current.Value.(Object)
			isSelected := selectsUnder(rec, environment)
			pointerNames := pointer.Names(recordedPointer(rec, filepath.Dir(environment)), environment)
			selects, names = isSelected, boolOrNil(pointerNames)
			protected = isSelected || pointerNames == nil || *pointerNames
			finishable = selectsEvery(rec, environment)
		}
		exclusive.Release()
	}
	occupied, _ := staging.DirectoryOccupied(environment)
	protectedValue, _ := protected.(bool)
	selectedValue, _ := selects.(bool)
	decision, _ := staging.Decide(left, staging.Dead, occupied, protected == nil || protectedValue, selectedValue)
	var recovery any
	switch {
	case settled:
	case contended:
		recovery = "wait for the run that holds " + path + record.LockSuffix + " before anything else. This call never took that lock, so it wrote nothing, and the other run may be writing this very claim"
	case !left.Usable():
		recovery = "make the claim at " + path + " readable or remove it, then run install again. The replacement itself finished and this runtime is in service, so it must not be deleted"
	case selects == nil:
		recovery = "read " + o.RecordPath + " before acting on this: what this host selects could not be established, so what the next run would do with this directory could not be either"
	case finishable && names != true:
		recovery = "read the owned pointer before rerunning: the host record selects this runtime but the pointer does not name it, so a rerun replaces that link before it writes the claim"
	case finishable:
		recovery = "clear whatever stopped the write at " + path + " (the error is in 'detail') and run install again with the same archive: it reads a selected runtime whose claim never settled as an interrupted promotion and finishes the bookkeeping, rebuilding and removing nothing"
	case protectedValue:
		recovery = "leave this directory alone: the host record no longer selects it, but something may still reach it"
	default:
		recovery = "nothing needs doing about this record, and do not rerun install to settle it: another run moved the selection on, and nothing selects this runtime or points at it now"
	}
	var detail any
	if err != nil {
		detail = err.Error()
	}
	return Object{
		field("path", path), field("settled", settled), field("released", err == nil), field("wanted", staging.Complete), field("detail", detail),
		field("readBack", Object{field("state", left.State), field("saying", nilIfEmpty(says)), field("detail", nilIfEmpty(left.Detail))}),
		field("inService", !staging.Removes(decision)),
		field("selection", Object{field("state", state), field("selects", selects), field("pointerNames", names), field("protected", protected), field("detail", snapshotDetail)}),
		field("recoveryRequires", recovery),
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// targetRecorded is runtime_install._target_is_recorded: whether a pointer target is a runtime
// directory some install entry of this record lives in.
func targetRecorded(rec Object, target string) bool {
	components, _ := record.Get(rec, "components").(Object)
	for _, c := range components {
		installs, _ := record.Get(asObject(c.Value), "installs").([]any)
		for _, raw := range installs {
			if environment := environmentOf(asObject(raw)); environment != "" {
				if resolved, err := record.Resolve(environment); err == nil && resolved == target {
					return true
				}
			}
		}
	}
	return false
}
