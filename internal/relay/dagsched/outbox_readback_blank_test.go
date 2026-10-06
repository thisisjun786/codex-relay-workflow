package dagsched

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-513: Linear changes the block it saves. A real save of a written block read back identical except for one blank line between the block's closing code fence and its end marker
// comment, so a confirmation that compared the written bytes was refused readback_mismatch for an unchanged block. The readback is compared with that blank line ignored and with nothing
// else ignored; these tests pin both halves. The Linear here is a double: it reproduces that observed change and makes no call.

// linearBlankBeforeBlockEnd is the one change Linear made to a saved block: a blank line between the closing fence line (three or more backticks) and the block's end marker.
func linearBlankBeforeBlockEnd(doc string) string {
	lines := strings.Split(doc, "\n")
	var out []string
	for i, line := range lines {
		if i > 0 && strings.HasPrefix(strings.TrimSpace(line), blockEndPrefix) {
			prev := strings.TrimSpace(lines[i-1])
			if len(prev) >= 3 && strings.Trim(prev, "`") == "" {
				out = append(out, "")
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func plainEntry(summary string) SummaryEntry {
	return SummaryEntry{SummaryID: "sum-0123", PlanID: "p1", ProjectKey: "P-TEST", Document: "doc-1", PlanRevision: 3, Seq: 2, StateDigest: "sd", SubjectDigest: "sj", Summary: summary, State: SummaryClaimed}
}

// The readback Linear returns confirms the entry, whatever the fence that holds the summary: the blank line is the only thing ignored, so the same holds with line-ending noise or more
// than one blank line there, and the block the document holds is reported as it is (its blank line kept).
func TestSummaryReconcileIgnoresTheBlankLineBeforeTheBlockEnd(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"a plain summary":                 "Plan p1, project P-TEST, revision 3, active\n\nStages\n  running 1: a\n\nNodes\n  a, CRW-a, stage running",
		"a body that quotes a fence":      "Plan p1\n\nNodes\n  a, one\n```\nquoted\n```\n  b, two",
		"a body that quotes a long fence": "Plan p1\n\n````\nx\n````\nend",
	}
	for name, body := range bodies {
		e := plainEntry(body)
		written := "# Coordination\n\n" + summaryContainer(e) + "\nAfter.\n"
		linear := linearBlankBeforeBlockEnd(written)
		if linear == written || !strings.Contains(linear, "\n\n"+blockEnd(e.SummaryID)) {
			t.Fatalf("%s: the double did not add Linear's blank line", name)
		}
		forms := map[string]string{
			"as written":                           written,
			"with Linear's blank line":             linear,
			"with two blank lines":                 strings.Replace(linear, "\n"+blockEnd(e.SummaryID), "\n\n"+blockEnd(e.SummaryID), 1),
			"with line endings and blanks as well": strings.ReplaceAll(linear, "\n", "  \r\n"),
		}
		for form, doc := range forms {
			rec := reconcileDocument(e, doc)
			if rec.Outcome != "already_written" || rec.Repair != "none" || rec.DocumentSummaryID != e.SummaryID || rec.Detail == "" {
				t.Errorf("%s, %s: %+v", name, form, rec)
			}
		}
		rec := reconcileDocument(e, linear)
		if want := linearBlankBeforeBlockEnd(canonText(summaryBlock(e))); rec.PreviousBlock != want {
			t.Errorf("%s: the block the document holds is not reported as it is:\n%q\nwant\n%q", name, rec.PreviousBlock, want)
		}
		// a confirmed entry whose document Linear saved is still carried by it: nothing is owed again
		e.State = SummaryConfirmed
		if rec := reconcileDocument(e, linear); rec.Outcome != "already_written" || rec.Again {
			t.Errorf("%s: a confirmed entry that the document carries is owed again: %+v", name, rec)
		}
	}
}

// complete confirms from the readback Linear returned, and keeps the block as the document held it.
func TestSummaryCompleteConfirmsTheReadbackLinearSaved(t *testing.T) {
	t.Parallel()
	f := summaryFixture(t)
	e, op, _ := claimedEntry(t, f, "doc-1")
	readback := linearBlankBeforeBlockEnd("# Coordination\n\nSome text.\n\n" + op.Container + "\nAfter.\n")
	if !strings.Contains(readback, "```\n\n"+blockEnd(e.SummaryID)) {
		t.Fatal("the double did not add Linear's blank line")
	}
	done, err := f.sched.CompleteSummary(context.Background(), e.SummaryID, "parent", reTokenFor(t, f, e.SummaryID), "doc-1", readback)
	if err != nil || done.Replayed || done.Entry.State != SummaryConfirmed || done.Entry.LastError != "" || done.Entry.Readback != linearBlankBeforeBlockEnd(op.Block) {
		t.Fatalf("complete %+v %v", done, err)
	}
	if got := entryBySeq(f, "doc-1", 1); got.State != SummaryConfirmed || got.Readback != done.Entry.Readback {
		t.Fatalf("the store holds %+v", got)
	}
}

// The parent's turn against a Linear whose save adds that blank line: the write lands once, the reconcile of the document that was saved says already_written, and complete confirms it.
// The next summary replaces the confirmed one the same way. Before CRW-513 the reconcile called the saved block stale and the parent rewrote the same container in a loop.
func TestSummaryParentFlowAgainstALinearThatAddsItsBlankLine(t *testing.T) {
	t.Parallel()
	f := summaryFixture(t)
	lin := newFakeLinear()
	ctx := context.Background()
	var firstID string
	for round := 1; round <= 2; round++ {
		if round == 2 {
			f.bump(1)
		}
		entry := newParentFlow(f, lin, "doc-1").enqueue().Entry
		claim, err := f.sched.ClaimSummary(ctx, entry.SummaryID, "parent")
		if err != nil {
			t.Fatal(err)
		}
		op := claim.Operation
		rec := reconcile(t, f, entry.SummaryID, lin.read())
		if rec.Repair == "initialize" {
			if err := lin.initialize(lin.read(), op.EmptyContainer); err != nil {
				t.Fatal(err)
			}
			rec = reconcile(t, f, entry.SummaryID, lin.read())
		}
		wantRelation := map[int]string{1: "", 2: "older"}[round]
		if rec.Repair != "replace_container" || rec.Relation != wantRelation {
			t.Fatalf("round %d: before the write the document reads %+v", round, rec)
		}
		if err := lin.patch(entry.Seq, containerIn(lin.read(), op.ContainerStart, op.ContainerEnd), op.Container); err != nil {
			t.Fatal(err)
		}
		lin.doc = linearBlankBeforeBlockEnd(lin.doc) // Linear's save normalises the markdown it stored
		if rec := reconcile(t, f, entry.SummaryID, lin.read()); rec.Outcome != "already_written" || rec.Repair != "none" {
			t.Fatalf("round %d: the saved document reads %+v", round, rec)
		}
		done, err := f.sched.CompleteSummary(ctx, entry.SummaryID, "parent", claim.Token, "doc-1", lin.read())
		if err != nil || done.Entry.State != SummaryConfirmed {
			t.Fatalf("round %d: complete %+v %v", round, done, err)
		}
		if lin.landed[entry.Seq] != 1 || lin.overwrites != 0 || strings.Count(lin.read(), "<!-- relay-dag-summary:") != 1 {
			t.Fatalf("round %d: writes %v, overwrites %d, document:\n%s", round, lin.landed, lin.overwrites, lin.read())
		}
		if round == 1 {
			firstID = entry.SummaryID
		}
	}
	// the first entry is confirmed and the document now holds the newer summary: that is a difference of content
	if rec := reconcile(t, f, firstID, lin.read()); rec.Outcome != "stale" || rec.Relation != "newer" {
		t.Fatalf("%+v", rec)
	}
}

// Only that blank line is ignored. Each readback below differs from the written block in content or in whitespace elsewhere, most of them with Linear's blank line as well, and is refused
// readback_mismatch: the entry stays claimed with the problem kept, and the readback that is right still confirms it afterwards.
func TestSummaryCompleteStillRefusesWhatLinearDidNotChange(t *testing.T) {
	t.Parallel()
	f := summaryFixture(t)
	e, op, _ := claimedEntry(t, f, "doc-1")
	end := blockEnd(e.SummaryID)
	page := "# Coordination\n\n"
	linear := linearBlankBeforeBlockEnd(page + op.Container + "\n")
	dropLine := func(prefix string) func(string) string {
		return func(doc string) string {
			var kept []string
			for _, line := range strings.Split(doc, "\n") {
				if !strings.HasPrefix(line, prefix) {
					kept = append(kept, line)
				}
			}
			return strings.Join(kept, "\n")
		}
	}
	replace := func(old, replacement string) func(string) string {
		return func(doc string) string { return strings.Replace(doc, old, replacement, 1) }
	}
	cases := []struct {
		name   string
		mutate func(string) string
	}{
		{"a header changed", replace("projectKey: P-TEST", "projectKey: P-TESX")},
		{"a line of the summary changed", replace("Nodes: ", "Nodez: ")},
		{"a line of the summary missing", dropLine("Denominator: ")},
		{"a line of the summary added", replace("\nStages\n", "\nStages\n  extra, stage running\n")},
		{"a non-blank line after the closing fence", replace("```\n\n"+end, "```\n\nstray\n"+end)},
		{"a non-blank line before the fence", replace("\n\n```text", "\n\nstray\n```text")},
		{"the closing fence gone", replace("\n```\n\n"+end, "\n\n"+end)},
		{"text between the block and the container end", replace("\n"+op.ContainerEnd, "\n\nstray paragraph\n"+op.ContainerEnd)},
		{"the blank line after the start marker", replace(blockStart(e.SummaryID)+"\n", blockStart(e.SummaryID)+"\n\n")},
		{"a blank line added inside the summary", replace("\nStages\n", "\n\nStages\n")},
		{"a blank line of the summary missing", replace("\n\nStages\n", "\nStages\n")},
		{"the blank line before the fence missing", replace("\n\n```text", "\n```text")},
		{"the end marker gone", replace("\n\n"+end, "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			readback := c.mutate(linear)
			if readback == linear {
				t.Fatal("the case changes nothing")
			}
			rec := reconcile(t, f, e.SummaryID, readback)
			if rec.Outcome == "already_written" {
				t.Fatalf("reconcile accepts it: %+v", rec)
			}
			_, err := f.sched.CompleteSummary(context.Background(), e.SummaryID, "parent", reTokenFor(t, f, e.SummaryID), "doc-1", readback)
			wantRefusal(t, err, contract.RefusalReadbackMismatch)
			if got := entryBySeq(f, "doc-1", 1); got.State != SummaryClaimed || got.LastError == "" || got.Readback != "" {
				t.Fatalf("after the refusal: %+v", got)
			}
		})
	}
	done, err := f.sched.CompleteSummary(context.Background(), e.SummaryID, "parent", reTokenFor(t, f, e.SummaryID), "doc-1", linear)
	if err != nil || done.Entry.State != SummaryConfirmed || done.Entry.LastError != "" {
		t.Fatalf("the readback that is right: %+v %v", done, err)
	}
}

// Through the binary: the readback Linear saved confirms (and reconcile reports the block with its blank line), and a real difference next to that blank line is refused.
func TestCLISummaryCompleteWithLinearsBlankLine(t *testing.T) {
	t.Parallel()
	saved := func(t *testing.T, mutate func(string) string) (state, id, token, page string) {
		t.Helper()
		state = summaryState(t)
		id = objectOf(t, summaryRun(t, state, 0, "dag-summary-enqueue", "--plan", "p1", "--actor", "parent", "--document", "doc-1"), "entry")["summary_id"].(string)
		claim := summaryRun(t, state, 0, "dag-summary-claim", "--summary", id, "--actor", "parent")
		token = claim["claim_token"].(string)
		page = t.TempDir() + "/doc.txt"
		readback := mutate(linearBlankBeforeBlockEnd("# page\n\n" + objectOf(t, claim, "operation")["container"].(string) + "\n"))
		if err := os.WriteFile(page, []byte(readback), 0o600); err != nil {
			t.Fatal(err)
		}
		return state, id, token, page
	}
	t.Run("confirmed", func(t *testing.T) {
		state, id, token, page := saved(t, func(s string) string { return s })
		rec := summaryRun(t, state, 0, "dag-summary-reconcile", "--summary", id, "--observed", "@"+page)
		if rec["outcome"] != "already_written" || rec["repair"] != "none" || !strings.Contains(rec["previous_block"].(string), "```\n\n<!-- /relay-dag-summary:"+id) {
			t.Fatalf("%v", rec)
		}
		out := summaryRun(t, state, 0, "dag-summary-complete", "--summary", id, "--actor", "parent", "--claim-token", token, "--document", "doc-1", "--readback", "@"+page)
		if e := objectOf(t, out, "entry"); e["state"] != "confirmed" || out["replayed"] != false {
			t.Fatalf("%v", out)
		}
	})
	t.Run("refused", func(t *testing.T) {
		state, id, token, page := saved(t, func(s string) string { return strings.Replace(s, "projectKey: ", "projectKey: x", 1) })
		out := summaryRun(t, state, 2, "dag-summary-complete", "--summary", id, "--actor", "parent", "--claim-token", token, "--document", "doc-1", "--readback", "@"+page)
		if out["reason"] != "readback_mismatch" {
			t.Fatalf("%v", out)
		}
	})
}
