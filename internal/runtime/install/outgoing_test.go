package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// outgoing is the selection the last promotion replaced, written only in the write that commits a
// promotion (or a rollback), so an update that fails before its promotion leaves it as it was
// and a bare rollback still returns to it. runtime_install.py gives the key another meaning: it
// writes the selection current when its install starts, right after its exclusive mkdir, and
// never puts it back when that install then fails. On a host where both installers run, such a
// failed Python install leaves outgoing naming the runtime already selected, and a bare rollback
// then refuses with nothing written; a rollback naming a directory the record lists still
// returns to it (decision 38).
func TestOutgoingIsWrittenOnlyByAPromotion(t *testing.T) {
	h := newHost(t)
	first, second, third := archive(t, "0.9.0", ""), archive(t, "0.9.1", ""), archive(t, "0.9.2", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	baseline := golden.Canon(at(h.hostRecord(t), "outgoing"))
	if at(h.hostRecord(t), "outgoing", "codex-thread-bridge", "selected") != filepath.Join(old, "bin") {
		t.Fatalf("outgoing after the update: %s", baseline)
	}

	o := h.options()
	o.Socket = filepath.Join(t.TempDir(), "nobody-listens.sock")
	if failed, code := install.Install(context.Background(), o, "update", install.Source{From: third}); code != install.Refused || at(failed, "failedStep") != "exercise the candidate" {
		t.Fatalf("the failing update: exit %d\n%s", code, golden.Canon(failed))
	}
	if got := golden.Canon(at(h.hostRecord(t), "outgoing")); got != baseline {
		t.Fatalf("an update that failed before its promotion rewrote outgoing:\n%s\nwas\n%s", got, baseline)
	}

	// runtime_install.py's stage write: hostrecord.update(outgoing=_outgoing_runtime(record)),
	// the selection current when its install started.
	selected := golden.Obj(at(h.hostRecord(t), "selected"))
	var stage record.Object
	for _, c := range []string{"codex-session-relay", "codex-thread-bridge"} {
		stage = append(stage, record.Object{{Key: c, Value: record.Object{
			{Key: "digest", Value: strings.Repeat("0", 64)}, {Key: "present", Value: true}, {Key: "selected", Value: record.Get(selected, c)},
		}}}...)
	}
	if _, err := record.Update(context.Background(), h.record, 1, record.Delta{Outgoing: &record.Outgoing{Value: stage}}); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, h.record)
	refused, code := install.Rollback(context.Background(), h.options(), "")
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "nothing to roll back to") {
		t.Fatalf("a bare rollback after a Python stage write: exit %d\n%s", code, golden.Canon(refused))
	}
	if readFile(t, h.record) != before || h.pointerTarget(t) != updated {
		t.Fatal("a refused rollback wrote something")
	}
	if back, code := install.Rollback(context.Background(), h.options(), old); code != install.OK || h.pointerTarget(t) != old {
		t.Fatalf("a rollback naming the directory: exit %d\n%s", code, golden.Canon(back))
	}
}

// Install entries are recorded before the candidate is exercised, as runtime_install.py records
// its entries before it measures, so a run killed during the exercise leaves entries for a
// candidate whose claim is STAGING. Nothing acts on them as a runtime: a rollback to that
// directory refuses (its claim never settled), a run of the same archive reclaims the directory
// and replaces them (one entry per location), and a run that then fails drops them with the
// directory it releases.
func TestAnUnsettledCandidatesInstallEntriesAreNeverActedOn(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, next := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	// Rewind to what a run killed during its exercise leaves: next unpacked with its entries
	// recorded and its claim STAGING, nobody holding its lock, the selection and the pointer on old.
	var back record.Object
	for _, c := range []string{"codex-session-relay", "codex-thread-bridge"} {
		back = append(back, record.Object{{Key: c, Value: filepath.Join(old, "bin")}}...)
	}
	if _, err := record.Update(context.Background(), h.record, 1, record.Delta{Select: back}); err != nil {
		t.Fatal(err)
	}
	if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
		t.Fatal(err)
	}
	write(t, staging.ClaimPath(next), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	if !listed(h.installsOf(t), next) {
		t.Fatal("the interrupted state carries no entries for the candidate")
	}

	before := readFile(t, h.record)
	refused, code := install.Rollback(context.Background(), h.options(), next)
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "not a runtime whose install finished") || h.pointerTarget(t) != old || readFile(t, h.record) != before {
		t.Fatalf("a rollback to an unsettled candidate: exit %d, pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(refused))
	}

	o := h.options()
	o.Socket = filepath.Join(t.TempDir(), "nobody-listens.sock")
	failed, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(failed, "failedStep") != "exercise the candidate" || !strings.HasPrefix(text(at(failed, "candidate")), "dropped") {
		t.Fatalf("a failing rerun: exit %d\n%s", code, golden.Canon(failed))
	}
	if _, err := os.Lstat(next); !os.IsNotExist(err) || listed(h.installsOf(t), next) {
		t.Fatal("a failed run left the unsettled candidate's directory or its entries")
	}

	h.mustInstall(t, "update", second)
	count := 0
	for _, environment := range h.installsOf(t) {
		if environment == next {
			count++
		}
	}
	if count != 1 || h.pointerTarget(t) != next {
		t.Fatalf("after the rerun the record lists %d entries for %s, pointer %s", count, next, h.pointerTarget(t))
	}
}

// A promotion records as outgoing the runtime the pointer leaves - what a host was reaching - as
// a moving rollback does: the selection, unless an interrupted move left the pointer on another
// recorded runtime, which is then the one a bare rollback returns the host to.
func TestAPromotionRecordsTheRuntimeThePointerLeaves(t *testing.T) {
	h := newHost(t)
	first, second, third := archive(t, "0.9.0", ""), archive(t, "0.9.1", ""), archive(t, "0.9.2", "")
	old, next := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	if err := pointer.Place(pointer.Path(h.dest), old); err != nil { // an interrupted move
		t.Fatal(err)
	}
	h.mustInstall(t, "update", third)
	if got := at(h.hostRecord(t), "outgoing", "codex-session-relay", "selected"); got != filepath.Join(old, "bin") {
		t.Fatalf("outgoing selects %v; the pointer left %s, and the record selected %s", got, old, next)
	}
	h.mustInstall(t, "update", archive(t, "0.9.3", ""))
	if got := at(h.hostRecord(t), "outgoing", "codex-thread-bridge", "selected"); got != filepath.Join(runtimeDir(h, "0.9.2", third, t), "bin") {
		t.Fatalf("with the pointer and the selection agreeing, outgoing selects %v", got)
	}
}
