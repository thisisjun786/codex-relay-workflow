package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The tests of CRW-282: the frozen copies a refused release leaves behind, and the recovery of a node whose managed start was released before it created a child. They drive the real
// store, the real managed engine and the scripted host of the release tests; helpers of this file are the only ones this change adds.

// largeB prepares plan rp so that the manifest of B is too large for a prompt (320 artifacts) and returns its manifest digest.
func (k *releaseKit) largeB() {
	k.t.Helper()
	releasePlan(k.fixture, "rp")
	k.acceptNode("rp", "A", acceptOpts{Artifacts: 320})
}

// copies are the frozen manifest copies under the children's artifact root.
func (k *releaseKit) copies() []string {
	matches, _ := filepath.Glob(filepath.Join(k.root, "dag-input-manifests", "*.json"))
	sort.Strings(matches)
	return matches
}

// journal is the detail of every journal row of a kind.
func (k *releaseKit) journal(kind string) []map[string]any {
	k.t.Helper()
	rows, err := k.s.DB.QueryContext(context.Background(), "SELECT subject, detail FROM journal WHERE kind = ? ORDER BY seq", kind)
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var subject, detail string
		if err := rows.Scan(&subject, &detail); err != nil {
			k.t.Fatal(err)
		}
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(detail), &entry); err != nil {
			k.t.Fatalf("journal detail %q: %v", detail, err)
		}
		entry["subject"] = subject
		out = append(out, entry)
	}
	return out
}

// abandon releases a node whose managed start never gets as far as the engine, then records the managed start as released: the state of a node whose start was released before it created a
// child. It returns the manifest digest and the request id of the abandoned intent.
func (k *releaseKit) abandon(plan, node string) (digest, request string) {
	k.t.Helper()
	real := k.sched.Start
	k.sched.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, context.DeadlineExceeded }
	_, err := k.release(plan, node)
	k.sched.Start = real
	if err == nil {
		k.t.Fatal("the start was expected to fail")
	}
	row, found, lerr := latestRelease(context.Background(), k.s.Q(context.Background()), plan, node)
	if lerr != nil || !found {
		k.t.Fatalf("no open intent after the failed start: %v %v", found, lerr)
	}
	n, _ := nodeOf(k.snapshot(plan), node)
	k.managedRow(row.Request, n.IssueKey, "released", "")
	return row.Digest, row.Request
}

func (k *releaseKit) closeRelease(plan, node, digest string) (CloseResult, error) {
	k.t.Helper()
	return k.sched.CloseRelease(context.Background(), plan, node, "parent", digest, "the operator released the start", k.requestFor(plan, node, digest))
}

// requestFor is the request id an operator names when closing the release of a manifest: the open intent's, or else the newest closed one of that digest (what a retry names).
func (k *releaseKit) requestFor(plan, node, digest string) string {
	k.t.Helper()
	if open, found, err := latestRelease(context.Background(), k.s.Q(context.Background()), plan, node); err != nil {
		k.t.Fatal(err)
	} else if found && open.Digest == digest {
		return open.Request
	}
	var request string
	if err := k.s.DB.QueryRow("SELECT abandoned_request_id FROM dag_release_recoveries WHERE plan_id = ? AND node_id = ? AND manifest_digest = ? AND action = 'closed' ORDER BY recorded_at DESC LIMIT 1", plan, node, digest).Scan(&request); err != nil {
		return "dag-none"
	}
	return request
}

func (k *releaseKit) mustClose(plan, node, digest string) CloseResult {
	k.t.Helper()
	res, err := k.closeRelease(plan, node, digest)
	if err != nil {
		k.t.Fatalf("close %s: %v", node, err)
	}
	return res
}

func (k *releaseKit) heldSlots() int {
	return k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND state = 'held'")
}

func (k *releaseKit) children() int {
	created, _ := k.host.counts()
	return created
}

func (k *releaseKit) recoveries(action string) int {
	return k.count("SELECT COUNT(*) FROM dag_release_recoveries WHERE action = ?", action)
}

// c1: a release refused after it froze an oversize manifest leaves nothing behind, and says what it removed.
func TestRefusedReleaseLeavesNoFrozenCopy(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	k.holdSlots(6)
	before := k.rows()
	_, err := k.release("rp", "B")
	if refusalReason(err) != "capacity_exhausted" {
		t.Fatalf("release = %v, want capacity_exhausted", err)
	}
	if got := k.copies(); len(got) != 0 {
		t.Fatalf("a refused release left %v", got)
	}
	removed := k.journal("dag_manifest_copy_removed")
	if len(removed) != 1 {
		t.Fatalf("journal rows of removed copies = %v", removed)
	}
	entry := removed[0]
	path, _ := entry["path"].(string)
	if entry["subject"] != "rp/B" || filepath.Dir(path) != filepath.Join(k.root, "dag-input-manifests") || filepath.Base(path) != entry["sha256"].(string)+".json" || entry["bytes"].(float64) < 60000 || entry["why"] != "capacity_exhausted" {
		t.Fatalf("evidence = %v", entry)
	}
	after := k.rows()
	if after.releases != before.releases || after.requests != before.requests || after.manifests != before.manifests || after.executions != before.executions || after.slots != before.slots {
		t.Fatalf("rows = %+v, want the %+v the setup left", after, before)
	}
	// capacity frees up and the retry binds: exactly one copy exists, the one the child's prompt names
	k.exec("DELETE FROM execution_slots WHERE subject_key LIKE 'held-%'")
	res := k.mustRelease("rp", "B")
	if !res.Bound {
		t.Fatalf("retry = %+v", res)
	}
	copies := k.copies()
	if len(copies) != 1 || !strings.Contains(k.host.sent[0], copies[0]) {
		t.Fatalf("copies after the retry = %v, the prompt names another file", copies)
	}
	if len(k.journal("dag_manifest_copy_removed")) != 1 {
		t.Fatal("the bound release removed a copy")
	}
}

// c1: assemble can refuse after it froze (the prompt is too long); that copy goes too.
func TestAssembleRefusalAfterTheFreezeRemovesTheCopy(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	req := k.request(false)
	req.Instructions = strings.Repeat("x", 95000)
	_, err := k.sched.Release(context.Background(), "rp", "B", "parent", req)
	if refusalReason(err) != "malformed_receipt" {
		t.Fatalf("release = %v, want malformed_receipt", err)
	}
	if got := k.copies(); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	if removed := k.journal("dag_manifest_copy_removed"); len(removed) != 1 || removed[0]["why"] != "malformed_receipt" {
		t.Fatalf("journal = %v", removed)
	}
	if k.children() != 0 || k.rows().releases != 0 {
		t.Fatal("a refused release started something")
	}
}

// c1: a release that loses a race over identical bytes created the file the winner reuses; the winner's intent names it, so it stays.
func TestLostRaceOverIdenticalBytesKeepsTheWinnersCopy(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	k.sched.Now = func() string { return "2026-10-02T01:00:00.000000+00:00" }
	var winner ReleaseResult
	var winnerErr error
	k.sched.testBetweenReadAndIntent = func() {
		k.sched.testBetweenReadAndIntent = nil
		winner, winnerErr = k.release("rp", "B")
	}
	res, err := k.release("rp", "B")
	if winnerErr != nil || !winner.Bound {
		t.Fatalf("the winner = %v %+v", winnerErr, winner)
	}
	if err != nil || !res.Bound || !res.Replayed || res.ChildTaskID != winner.ChildTaskID {
		t.Fatalf("the loser = %v %+v, want the winner's child replayed", err, res)
	}
	copies := k.copies()
	if len(copies) != 1 || !strings.Contains(k.host.sent[0], copies[0]) {
		t.Fatalf("copies = %v: the winner's prompt must still find its file", copies)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "relied_on" {
		t.Fatalf("journal kept = %v", kept)
	}
	if k.children() != 1 {
		t.Fatalf("%d children", k.children())
	}
}

// c1: the intent is recorded over a copy that is there: a copy that vanished between the freeze and the intent transaction is frozen again inside it.
func TestIntentTransactionFreezesAVanishedCopyAgain(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	k.sched.testAfterReserve = func() error {
		for _, c := range k.copies() {
			if err := os.Remove(c); err != nil {
				return err
			}
		}
		return nil
	}
	res := k.mustRelease("rp", "B")
	if !res.Bound {
		t.Fatalf("release = %+v", res)
	}
	copies := k.copies()
	if len(copies) != 1 || !strings.Contains(k.host.sent[0], copies[0]) {
		t.Fatalf("copies = %v, the prompt of the bound child names a file that is not there", copies)
	}
	raw, err := os.ReadFile(copies[0])
	if err != nil || shaOf(raw)+".json" != filepath.Base(copies[0]) {
		t.Fatalf("the copy is not what its name says: %v", err)
	}
}

// c1: what a live intent or a stored manifest names is never removed, whoever asks: a bound release, the manifest an acceptance rests on, a prepared correction.
func TestDiscardNeverRemovesWhatALiveIntentOrAStoredManifestNames(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	res := k.mustRelease("rp", "B")
	bound := k.copies()
	if len(bound) != 1 {
		t.Fatalf("copies = %v", bound)
	}
	raw, _ := os.ReadFile(bound[0])
	c := &frozenCopy{Root: k.root, Path: bound[0], SHA: shaOf(raw), Canonical: raw, Created: true}
	// a bound release names it
	if removed, err := k.sched.discardFrozenCopy(context.Background(), c, "rp", "B", "test"); err != nil || removed {
		t.Fatalf("discard of a bound release's copy = %v %v", removed, err)
	}
	// the node is accepted afterwards: still named
	k.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, ack_tier, verdict_turn_id,"+
		" rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?, 'rp', 'B', ?, ?, 1, 'evt', ?, ?, 'verified', 'host_read', 'turn', '{}', 'parent', 0, 't', 'active')",
		dig("acc"), res.ManifestDigest, res.RelationshipID, dig("rev"), dig("crit"))
	if removed, err := k.sched.discardFrozenCopy(context.Background(), c, "rp", "B", "test"); err != nil || removed {
		t.Fatalf("discard after acceptance = %v %v", removed, err)
	}
	if _, err := os.Stat(bound[0]); err != nil {
		t.Fatalf("the copy of an accepted node is gone: %v", err)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 2 {
		t.Fatalf("journal kept = %v", kept)
	}
}

func TestDiscardNeverRemovesAPreparedCorrectionsCopy(t *testing.T) {
	k := newReleaseKit(t)
	k.correctionKit()
	prepared := k.prepare()
	raw, err := os.ReadFile(prepared.FrozenPath)
	if err != nil {
		t.Fatal(err)
	}
	c := &frozenCopy{Root: k.root, Path: prepared.FrozenPath, SHA: shaOf(raw), Canonical: raw, Created: true}
	if removed, err := k.sched.discardFrozenCopy(context.Background(), c, "rp", "A", "test"); err != nil || removed {
		t.Fatalf("discard of a prepared correction's copy = %v %v", removed, err)
	}
	if _, err := os.Stat(prepared.FrozenPath); err != nil {
		t.Fatalf("the prepared correction's copy is gone: %v", err)
	}
}

// c1: a call removes only a file it created, only a regular file that hashes to its name, and nothing a live intent names; an orphan is removed with its evidence.
func TestDiscardRemovesOnlyAnOrphanItCreated(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	dir := filepath.Join(k.root, "dag-input-manifests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical := []byte(strings.Repeat("orphan ", 100))
	path, created, err := freezeManifestCopy(k.root, canonical)
	if err != nil || !created || path != filepath.Join(dir, shaOf(canonical)+".json") {
		t.Fatalf("freeze = %q %v %v", path, created, err)
	}
	if again, createdAgain, err := freezeManifestCopy(k.root, canonical); err != nil || createdAgain || again != path {
		t.Fatalf("a second freeze of the same bytes = %q %v %v: it reused the file and must not claim it", again, createdAgain, err)
	}
	// a file this call only reused is not its to remove
	reused := &frozenCopy{Root: k.root, Path: path, SHA: shaOf(canonical), Canonical: canonical, Created: false}
	if removed, err := k.sched.discardFrozenCopy(context.Background(), reused, "rp", "B", "test"); err != nil || removed {
		t.Fatalf("discard of a reused file = %v %v", removed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a file the call did not create was removed")
	}
	// a file whose bytes are not what its name says is not a manifest copy of this call
	tampered := &frozenCopy{Root: k.root, Path: path, SHA: shaOf([]byte("something else")), Canonical: canonical, Created: true}
	if removed, err := k.sched.discardFrozenCopy(context.Background(), tampered, "rp", "B", "test"); err == nil && removed {
		t.Fatal("a file that does not hash to the name was removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a mislabelled file was removed")
	}
	// an orphan the call created goes, with its evidence
	orphan := &frozenCopy{Root: k.root, Path: path, SHA: shaOf(canonical), Canonical: canonical, Created: true}
	if removed, err := k.sched.discardFrozenCopy(context.Background(), orphan, "rp", "B", "capacity_exhausted"); err != nil || !removed {
		t.Fatalf("discard of an orphan = %v %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the orphan is still there: %v", err)
	}
	if entries := k.journal("dag_manifest_copy_removed"); len(entries) != 1 || entries[0]["sha256"] != shaOf(canonical) {
		t.Fatalf("evidence = %v", entries)
	}
	// a link planted where the copy was is never followed
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if removed, _ := k.sched.discardFrozenCopy(context.Background(), orphan, "rp", "B", "test"); removed {
		t.Fatal("a link was treated as a copy")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("the file behind a planted link was removed")
	}
}

// c2: the reading names the way on, and closing returns the slot and frees the node (it reads ready again, since nothing owns it).
func TestCloseReturnsTheSlotOfAnAbandonedRelease(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, request := k.abandon("rp", "A")
	n := k.read("rp").node("A")
	if n.State != StateReleasing || n.Reason != BlockedReleaseAbandoned || !strings.Contains(n.Detail, "dag-release-close") {
		t.Fatalf("the abandoned node reads %+v, want blocked:release_abandoned pointing at dag-release-close", n)
	}
	if k.heldSlots() != 1 {
		t.Fatalf("held slots = %d", k.heldSlots())
	}
	res := k.mustClose("rp", "A", digest)
	if res.RequestID != request || res.ManifestDigest != digest || res.Replayed || !res.SlotReleased || res.SlotID == "" || res.SuccessorRequestID != "" {
		t.Fatalf("close = %+v", res)
	}
	if k.heldSlots() != 0 || k.recoveries("closed") != 1 {
		t.Fatalf("held slots %d, closed rows %d", k.heldSlots(), k.recoveries("closed"))
	}
	var reason string
	if err := k.s.DB.QueryRow("SELECT release_reason FROM execution_slots WHERE subject_key = ?", SlotSubjectKey("rp", "A")).Scan(&reason); err != nil || reason != "dag_release_closed" {
		t.Fatalf("the slot was returned with reason %q (%v)", reason, err)
	}
	if n := k.read("rp").node("A"); n.State != StateReady || n.Disposition != DispReady {
		t.Fatalf("after the close the node reads %+v, want it ready again (nothing owns it)", n)
	}
	if k.children() != 0 {
		t.Fatal("closing created a child")
	}
}

// c2: the same close again is the same answer, before and after the node was released again, and it changes nothing.
func TestCloseIsIdempotent(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, _ := k.abandon("rp", "A")
	first := k.mustClose("rp", "A", digest)
	before := k.rows()
	second := k.mustClose("rp", "A", digest)
	want := first
	want.Replayed = true
	if second != want {
		t.Fatalf("second close = %+v, want %+v", second, want)
	}
	if k.rows() != before || k.recoveries("closed") != 1 || k.heldSlots() != 0 {
		t.Fatalf("a repeated close changed the store: %+v -> %+v", before, k.rows())
	}
	// the node is released again; the recorded closure is still the answer, and it names the successor
	released := k.mustRelease("rp", "A")
	third, err := k.sched.CloseRelease(context.Background(), "rp", "A", "parent", digest, "the operator released the start", first.RequestID)
	if err != nil || !third.Replayed || third.RequestID != first.RequestID || third.SuccessorRequestID != released.RequestID || third.SlotID != first.SlotID || !third.SlotReleased {
		t.Fatalf("close after the reopening = %+v (reopened as %s)", third, released.RequestID)
	}
	if k.children() != 1 || k.heldSlots() != 1 {
		t.Fatalf("children %d, held %d", k.children(), k.heldSlots())
	}
}

// c2: close answers only for an intent that is abandoned.
func TestCloseRefusals(t *testing.T) {
	type setup func(k *releaseKit) string
	cases := []struct {
		name    string
		setup   setup
		node    string
		digest  string // "" is the digest setup returned
		actor   string
		reason  string
		request string // "" is the node's current open request (or none); "-" is no request at all
		want    string
	}{
		{name: "a running child", setup: func(k *releaseKit) string { return k.mustRelease("rp", "A").ManifestDigest }, node: "A", want: "disposition_conflict"},
		{name: "a start in flight whose creation is unknown", setup: func(k *releaseKit) string {
			k.host.loseFirstCreation = true
			res, err := k.release("rp", "A")
			if err != nil || res.Bound {
				k.t.Fatalf("first call = %v %+v", err, res)
			}
			return res.ManifestDigest
		}, node: "A", want: "disposition_conflict"},
		{name: "another manifest than the intent's", setup: func(k *releaseKit) string { d, _ := k.abandon("rp", "A"); _ = d; return dig("another manifest") }, node: "A", want: "disposition_conflict"},
		{name: "a node that was never released", setup: func(k *releaseKit) string { return dig("anything") }, node: "B", want: "disposition_conflict"},
		{name: "a request that is not the node's", setup: func(k *releaseKit) string { d, _ := k.abandon("rp", "A"); return d }, node: "A", request: "dag-none", want: "disposition_conflict"},
		{name: "no request", setup: func(k *releaseKit) string { d, _ := k.abandon("rp", "A"); return d }, node: "A", request: "-", want: "malformed_receipt"},
		{name: "a node the plan does not have", setup: func(k *releaseKit) string { return dig("anything") }, node: "ghost", want: "unregistered_scope"},
		{name: "an actor that is not the project's parent", setup: func(k *releaseKit) string { d, _ := k.abandon("rp", "A"); return d }, node: "A", actor: "someone-else", want: "scope_role_mismatch"},
		{name: "no reason", setup: func(k *releaseKit) string { d, _ := k.abandon("rp", "A"); return d }, node: "A", reason: "-", want: "malformed_receipt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			digest := c.setup(k)
			if c.digest != "" {
				digest = c.digest
			}
			actor, reason := "parent", "the operator released the start"
			if c.actor != "" {
				actor = c.actor
			}
			if c.reason == "-" {
				reason = "  "
			}
			rows, held := k.rows(), k.heldSlots()
			request := c.request
			switch request {
			case "":
				request = "dag-none"
				if open, found, err := latestRelease(context.Background(), k.s.Q(context.Background()), "rp", c.node); err != nil {
					t.Fatal(err)
				} else if found {
					request = open.Request
				}
			case "-":
				request = " "
			}
			_, err := k.sched.CloseRelease(context.Background(), "rp", c.node, actor, digest, reason, request)
			if refusalReason(err) != c.want {
				t.Fatalf("close = %v, want %s", err, c.want)
			}
			if k.rows() != rows || k.heldSlots() != held || k.recoveries("closed") != 0 {
				t.Fatalf("a refused close changed the store: %+v -> %+v, held %d -> %d", rows, k.rows(), held, k.heldSlots())
			}
		})
	}
}

// c2: after the close the node is released again by the ordinary command. The same manifest goes under a successor request id (the abandoned one is a tombstone forever), once.
func TestReleaseAfterCloseReleasesOnceUnderASuccessorRequest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, abandoned := k.abandon("rp", "A")
	k.mustClose("rp", "A", digest)
	res := k.mustRelease("rp", "A")
	if !res.Bound || res.Replayed || res.ManifestDigest != digest || res.RequestID == abandoned || res.RequestID != RecoveryRequestID("rp", "A", digest, abandoned) || res.RequestID == "" {
		t.Fatalf("release after close = %+v (abandoned request %s)", res, abandoned)
	}
	if k.children() != 1 || k.heldSlots() != 1 {
		t.Fatalf("children %d, held slots %d", k.children(), k.heldSlots())
	}
	var bound string
	if err := k.s.DB.QueryRow("SELECT managed_request_id FROM dag_node_executions").Scan(&bound); err != nil || bound != res.RequestID {
		t.Fatalf("the child is bound under %q (%v)", bound, err)
	}
	rows := k.rows()
	if rows.releases != 1 || rows.requests != 1 || rows.executions != 1 || k.recoveries("closed") != 1 || k.recoveries("rereleased") != 1 {
		t.Fatalf("rows = %+v, closed %d, rereleased %d", rows, k.recoveries("closed"), k.recoveries("rereleased"))
	}
	if n := k.read("rp").node("A"); n.Disposition != DispSkip || n.Reason != SkipAlreadyOwned {
		t.Fatalf("the released node reads %+v", n)
	}
	// the same call again is a replay of the same child
	again := k.mustRelease("rp", "A")
	if !again.Bound || !again.Replayed || again.ChildTaskID != res.ChildTaskID || again.RequestID != res.RequestID || again.RelationshipID != res.RelationshipID {
		t.Fatalf("repeat = %+v, want the same child", again)
	}
	if k.children() != 1 || k.rows() != rows || k.heldSlots() != 1 {
		t.Fatalf("a repeat changed the store: children %d, rows %+v -> %+v", k.children(), rows, k.rows())
	}
}

// c2: a manifest that changed since the abandoned release (here the tip the child starts from moved) is an ordinary new release under its own digest.
func TestReleaseAfterCloseOfAChangedManifestIsAnOrdinaryRelease(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, _ := k.abandon("rp", "I")
	k.mustClose("rp", "I", digest)
	k.tips.sha = "3333333333333333333333333333333333333333"
	res := k.mustRelease("rp", "I")
	if !res.Bound || res.ManifestDigest == digest || res.RequestID != ReleaseRequestID("rp", "I", res.ManifestDigest) {
		t.Fatalf("release = %+v, want a new manifest under the ordinary request id", res)
	}
	if k.recoveries("rereleased") != 0 || k.rows().releases != 2 || k.children() != 1 {
		t.Fatalf("rereleased %d, releases %d, children %d", k.recoveries("rereleased"), k.rows().releases, k.children())
	}
}

// c2: a digest that matches an OLDER closed intent (the tip moved away and back) is reopened under a successor of that intent's request, and the node's current intent is the successor.
func TestReleaseAfterCloseOfAnOlderManifestReopensThatIntent(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	first, firstRequest := k.abandon("rp", "I")
	k.mustClose("rp", "I", first)
	k.tips.sha = "3333333333333333333333333333333333333333"
	second, _ := k.abandon("rp", "I")
	if second == first {
		t.Fatal("the moved tip did not change the manifest")
	}
	k.mustClose("rp", "I", second)
	k.tips.sha = head1
	res := k.mustRelease("rp", "I")
	if !res.Bound || res.ManifestDigest != first || res.RequestID != RecoveryRequestID("rp", "I", first, firstRequest) {
		t.Fatalf("release = %+v, want the first manifest again under a successor of %s", res, firstRequest)
	}
	row, found, err := latestRelease(context.Background(), k.s.Q(context.Background()), "rp", "I")
	if err != nil || !found || row.Request != res.RequestID {
		t.Fatalf("current intent = %+v %v %v, want the successor", row, found, err)
	}
	if again := k.mustRelease("rp", "I"); !again.Replayed || again.ChildTaskID != res.ChildTaskID || k.children() != 1 {
		t.Fatalf("repeat = %+v, children %d", again, k.children())
	}
}

// c2: nothing is released again while a live child owns the issue, and the refusal leaves no slot and no row behind.
func TestReleaseAfterCloseCreatesNoChildWhileALiveChildExists(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, _ := k.abandon("rp", "A")
	k.mustClose("rp", "A", digest)
	k.foreignRelationship("CRW-A")
	rows := k.rows()
	_, err := k.release("rp", "A")
	if refusalReason(err) != "duplicate_assignment" {
		t.Fatalf("release = %v, want duplicate_assignment", err)
	}
	if k.children() != 0 || k.heldSlots() != 0 || k.recoveries("rereleased") != 0 || k.rows() != rows {
		t.Fatalf("children %d, held %d, rereleased %d, rows %+v -> %+v", k.children(), k.heldSlots(), k.recoveries("rereleased"), rows, k.rows())
	}
}

// c2: eight callers release a closed node at once, from two connections: one child.
func TestReleaseAfterCloseCreatesOneChildUnderRace(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, _ := k.abandon("rp", "A")
	k.mustClose("rp", "A", digest)
	other, err := store.Open(context.Background(), k.path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second := &Scheduler{Store: other, Now: k.clock}
	k.wire(second)
	var wg sync.WaitGroup
	results := make([]ReleaseResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := k.sched
			if i%2 == 1 {
				s = second
			}
			results[i], errs[i] = s.Release(context.Background(), "rp", "A", "parent", k.request(false))
		}(i)
	}
	wg.Wait()
	children := map[string]bool{}
	for i, res := range results {
		if errs[i] != nil || !res.Bound {
			t.Fatalf("call %d = %v %+v", i, errs[i], res)
		}
		children[res.ChildTaskID] = true
	}
	if k.children() != 1 || len(children) != 1 || k.recoveries("rereleased") != 1 || k.heldSlots() != 1 || k.rows().executions != 1 {
		t.Fatalf("children %d (%v), rereleased %d, held %d, executions %d", k.children(), children, k.recoveries("rereleased"), k.heldSlots(), k.rows().executions)
	}
}

// c2: a successor start that is released again is abandoned again: close and release once more give a third request id, and still one live child.
func TestAbandonedSuccessorCanBeClosedAndReleasedAgain(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, first := k.abandon("rp", "A")
	k.mustClose("rp", "A", digest)
	again, second := k.abandon("rp", "A")
	if again != digest || second == first || second != RecoveryRequestID("rp", "A", digest, first) {
		t.Fatalf("the reopened intent is %s under %s (first %s)", again, second, first)
	}
	if n := k.read("rp").node("A"); n.Reason != BlockedReleaseAbandoned {
		t.Fatalf("the abandoned successor reads %+v", n)
	}
	closed := k.mustClose("rp", "A", digest)
	if closed.RequestID != second || closed.Replayed || !closed.SlotReleased {
		t.Fatalf("second close = %+v", closed)
	}
	res := k.mustRelease("rp", "A")
	if !res.Bound || res.RequestID != RecoveryRequestID("rp", "A", digest, second) {
		t.Fatalf("release = %+v", res)
	}
	if k.children() != 1 || k.heldSlots() != 1 || k.recoveries("closed") != 2 || k.recoveries("rereleased") != 2 {
		t.Fatalf("children %d, held %d, closed %d, rereleased %d", k.children(), k.heldSlots(), k.recoveries("closed"), k.recoveries("rereleased"))
	}
}

// c2: an operator may already have returned the slot; closing then returns nothing and still closes.
func TestCloseAfterTheOperatorReturnedTheSlot(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	digest, _ := k.abandon("rp", "A")
	if _, err := (&capacity.Capacity{Store: k.s, Now: k.clock}).Release(context.Background(), capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ReleasedBy: "parent", Reason: "operator"}); err != nil {
		t.Fatal(err)
	}
	res := k.mustClose("rp", "A", digest)
	if res.SlotReleased || res.SlotID != "" || k.recoveries("closed") != 1 {
		t.Fatalf("close = %+v", res)
	}
	if released := k.mustRelease("rp", "A"); !released.Bound || k.heldSlots() != 1 || k.children() != 1 {
		t.Fatalf("release = %+v, held %d, children %d", released, k.heldSlots(), k.children())
	}
}

// c1: closing removes the copy the abandoned intent froze, unless a live intent still names the same file; the answer records it and a repeat answers the same.
func TestCloseRemovesTheCopyOfTheAbandonedIntent(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	copies := k.copies()
	if len(copies) != 1 {
		t.Fatalf("copies before the close = %v", copies)
	}
	res := k.mustClose("rp", "B", digest)
	if res.Copy == nil || !res.Copy.Removed || res.Copy.Path != copies[0] || len(k.copies()) != 0 {
		t.Fatalf("close = %+v, copies %v", res, k.copies())
	}
	if removed := k.journal("dag_manifest_copy_removed"); len(removed) != 1 || removed[0]["why"] != "closed" {
		t.Fatalf("journal = %v", removed)
	}
	if again := k.mustClose("rp", "B", digest); !again.Replayed || again.Copy == nil || *again.Copy != *res.Copy {
		t.Fatalf("repeat = %+v", again)
	}
	released := k.mustRelease("rp", "B")
	if c := k.copies(); !released.Bound || len(c) != 1 || !strings.Contains(k.host.sent[0], c[0]) {
		t.Fatalf("release after the close = %+v, copies %v", released, k.copies())
	}
}

func TestCloseKeepsACopyALiveIntentNames(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	digest, _ := k.abandon("rp", "B")
	copies := k.copies()
	if len(copies) != 1 {
		t.Fatalf("copies = %v", copies)
	}
	// another plan's intent, not closed, whose frozen request names the very same file
	releasePlan(k.fixture, "rq")
	other := dig("another manifest")
	k.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES ('rq', 'B', ?, ?, 0, 't')", other, ReleaseRequestID("rq", "B", other))
	k.exec("INSERT INTO dag_release_requests (plan_id, node_id, manifest_digest, request_sha256, request_json, marker_root, socket, state_selector, recorded_at) VALUES ('rq', 'B', ?, ?, ?, 'm', 's', 'x', 't')",
		other, dig("request"), "{\"prompt\":\"stored at "+copies[0]+" (sha256 x)\"}")
	res := k.mustClose("rp", "B", digest)
	if res.Copy == nil || res.Copy.Removed {
		t.Fatalf("close = %+v, want the copy kept", res)
	}
	if _, err := os.Stat(copies[0]); err != nil {
		t.Fatalf("the copy a live intent names is gone: %v", err)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "relied_on" {
		t.Fatalf("journal = %v", kept)
	}
}

// newReleaseKitAt is newReleaseKit over a store at a given state directory, for the tests that run the built binary.
func newReleaseKitAt(t *testing.T, state string) *releaseKit {
	t.Helper()
	f := newFixtureAt(t, filepath.Join(state, "relay.sqlite3"))
	root := t.TempDir()
	settings := map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}, "approvalPolicy": "never", "cwd": root, "runtimeWorkspaceRoots": []any{root}, "model": "gpt-5", "reasoningEffort": "medium", "environments": []any{}}
	k := &releaseKit{fixture: f, host: newScriptedHost(t, settings), tips: &tips{sha: head1}, forge: &prs{by: map[string]PullRequest{}}, root: root, marker: t.TempDir(), state: t.TempDir()}
	f.projectParent()
	k.wire(f.sched)
	return k
}

// c2: the command through the built binary: the shapes of success, replay, refusal and usage.
func TestCLIReleaseClose(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	k := newReleaseKitAt(t, state)
	releasePlan(k.fixture, "rp")
	digest, request := k.abandon("rp", "A")
	if err := k.s.Close(); err != nil {
		t.Fatal(err)
	}
	args := func(manifest string, extra ...string) []string {
		return append([]string{"dag-release-close", "--plan", "rp", "--node", "A", "--actor", "parent", "--manifest", manifest, "--request-id", request}, extra...)
	}
	out, code := crw(t, state, args(digest, "--reason", "the operator released the start")...)
	m := parseOut(t, out)
	if code != 0 || m["schema"] != "dag-release-close/1" || m["ok"] != true || m["closed"] != true || m["request_id"] != request || m["replayed"] != false || m["slot_released"] != true || m["manifest_digest"] != digest {
		t.Fatalf("close: exit %d\n%s", code, out)
	}
	out, code = crw(t, state, args(digest, "--reason", "the operator released the start")...)
	again := parseOut(t, out)
	if code != 0 || again["replayed"] != true || again["request_id"] != request || again["slot_id"] != m["slot_id"] {
		t.Fatalf("repeat: exit %d\n%s", code, out)
	}
	out, code = crw(t, state, args(dig("another"), "--reason", "x")...)
	if refused := parseOut(t, out); code != 2 || refused["reason"] != "disposition_conflict" {
		t.Fatalf("another manifest: exit %d\n%s", code, out)
	}
	// a missing required option is the argparse usage error (exit 2, nothing on stdout), and nothing is written
	if out, code = crw(t, state, args(digest)...); code != 2 || out != "" {
		t.Fatalf("without --reason: exit %d\n%s", code, out)
	}
	if out, code = crw(t, state, "dag-release-close", "--plan", "rp", "--node", "A", "--actor", "parent", "--manifest", digest, "--reason", "x"); code != 2 || out != "" {
		t.Fatalf("without --request-id: exit %d\n%s", code, out)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var closed, held int
	if err := db.QueryRow("SELECT COUNT(*) FROM dag_release_recoveries WHERE action = 'closed'").Scan(&closed); err != nil || closed != 1 {
		t.Fatalf("closed rows = %d (%v)", closed, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM execution_slots WHERE state = 'held'").Scan(&held); err != nil || held != 0 {
		t.Fatalf("held slots = %d (%v)", held, err)
	}
}

// c3: the page names the command, the table and the journal evidence it relies on.
func TestSchedulerPageDescribesTheRecoveryAndTheCleanup(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, name := range []string{"dag-release-close", "dag_release_recoveries", "dag_manifest_copy_removed", "dag_manifest_copy_kept", "dag_release_closed"} {
		if !strings.Contains(page, name) {
			t.Errorf("docs/relay/dag-scheduler.md does not mention %s", name)
		}
	}
}
