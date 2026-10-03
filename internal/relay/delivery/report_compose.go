package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// This file owns what the two work-report messages share: the parent's verification request
// (composeWorkCompletion, report_render.go) and the child's revision request (composeWorkRevision,
// report_revision.go). Both are lists of reportSection that reportCompose fits to a byte budget.
// The CXC status table, the status lines under a header, the unresolved block, the workflow
// restore block and the message-id derivation are defined once, here.
//
// What stays with each composition is what its bytes spell differently: the evidence lines
// ("verification:" against "PROOF:"), the repository, base and head lines, the relay record and
// each message's own title and summary line.

// reportSection is one block of a message. rank orders the blocks reportCompose drops when the
// message is over its budget (higher first, optional blocks only); keep is how many of an essential
// block's first lines survive a cut; last marks the block that stays after the omission line.
type reportSection struct {
	name       string
	lines      []string
	rank, keep int
	essential  bool
	last       bool
}

// cxcStatusMeaning says what each CXC report status claims. Both messages print it under the
// status line; a status that is not one of the six reads as an empty meaning.
var cxcStatusMeaning = map[string]string{"DONE": "the child proved every recorded criterion against its own work", "NOOP": "nothing needed doing, and the finding that established that is the deliverable", "BLOCKED": "an external dependency is in the way", "UNSAFE": "a human risk decision is required before this can proceed", "NEEDS_HUMAN": "a judgment only the user can make", "BUDGET_EXHAUSTED": "a bound the plan actually stated ran out; best-so-far is adopted"}

// cxcStatusLines are the two lines every message header carries after its summary: the CXC
// status with its reason, and what that status means.
func cxcStatusLines(r map[string]any) []string {
	return []string{
		"cxc: " + reportValue(r, "cxc_status") + " - " + reportValue(r, "cxc_reason"),
		"  meaning: " + cxcStatusMeaning[reportValue(r, "cxc_status")],
	}
}

// reportHeaderSection is the header both messages open with: the message's own title and summary
// line, the status lines, then whatever lines only that message adds. It is never dropped or cut.
func reportHeaderSection(title, summary string, r map[string]any, tail ...string) reportSection {
	lines := append([]string{title, summary}, cxcStatusLines(r)...)
	lines = append(lines, tail...)
	return reportSection{"header", lines, 0, len(lines), true, false}
}

// unresolvedLines are the unresolved block of a report, or its "none" line.
func unresolvedLines(r map[string]any) []string {
	unresolved := []string{"", "unresolved: none"}
	if items := py.Items(r["unresolved"]); len(items) > 0 {
		unresolved = []string{"", "unresolved:"}
		for _, raw := range items {
			if v, ok := raw.(string); ok {
				unresolved = append(unresolved, "  - "+unheaded(v))
			} else {
				v := py.Dict(raw, false)
				unresolved = append(unresolved, strings.TrimRight("  - "+unheaded(pyvalue.Str(v["id"]))+": "+pyvalue.Str(py.Or(v["note"], "")), ": "))
			}
		}
	}
	return unresolved
}

func unresolvedSection(r map[string]any) reportSection {
	return reportSection{"unresolved", unresolvedLines(r), 2, 2, true, false}
}

func restoreSection(r map[string]any) reportSection {
	return reportSection{"workflow restore", workRestoreLines(r), 3, 0, false, false}
}

// reportMessageID is the id of a relay message: the first 32 hex digits of the sha256 of its
// direction, relationship, purpose and event.
func reportMessageID(direction, rid, purpose, event string) string {
	digest := sha256.Sum256([]byte(direction + "|" + rid + "|" + purpose + "|" + event))
	return hex.EncodeToString(digest[:])[:32]
}

// reportCompose fits the sections to the byte budget. It first drops whole optional sections,
// highest rank first, then cuts the tail of essential sections down to their keep count, each cut
// ending in a "... N more" line; it never removes a section's first keep lines, and a refusal
// follows when even that does not fit. The omission line goes before the section marked last.
func reportCompose(sections []reportSection, event string, budget int) (string, error) {
	blocks := make([][]string, len(sections))
	order := make([]int, len(sections))
	removed := []string{}
	lineCount, totalBytes := 0, 0
	for _, section := range sections {
		lineCount += len(section.lines)
		for _, line := range section.lines {
			totalBytes += len(line)
		}
	}
	tail := -1
	for i, section := range sections {
		blocks[i] = append([]string{}, section.lines...)
		order[i] = i
		if section.last {
			tail = i
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return sections[order[i]].rank > sections[order[j]].rank })
	omission := func() string {
		return "omitted: " + strings.Join(removed, ", ") + " - read in full with codex-session-relay show --event " + event
	}
	rendered := func() string {
		lines := []string{}
		for i, block := range blocks {
			if i != tail {
				lines = append(lines, block...)
			}
		}
		if len(removed) > 0 {
			lines = append(lines, "", omission())
		}
		if tail >= 0 {
			lines = append(lines, blocks[tail]...)
		}
		return strings.Join(lines, "\n")
	}
	overBudget := func() bool {
		count, total := lineCount, totalBytes
		if len(removed) > 0 {
			count += 2
			total += len(omission())
		}
		return total+max(count-1, 0) > budget
	}
	for _, i := range order {
		if !overBudget() {
			break
		}
		if sections[i].essential || len(blocks[i]) == 0 {
			continue
		}
		removed = append(removed, sections[i].name)
		lineCount -= len(blocks[i])
		for _, line := range blocks[i] {
			totalBytes -= len(line)
		}
		blocks[i] = nil
	}
	for _, i := range order {
		section := sections[i]
		if !section.essential || len(section.lines) <= section.keep {
			continue
		}
		kept := len(section.lines)
		markerBytes := 0
		for overBudget() && kept > section.keep {
			kept--
			lineCount--
			totalBytes -= len(section.lines[kept])
			marker := fmt.Sprintf("  ... %d more, see the full record", len(section.lines)-kept)
			totalBytes += len(marker) - markerBytes
			if markerBytes == 0 {
				lineCount++
			}
			markerBytes = len(marker)
			blocks[i] = append(append([]string{}, section.lines[:kept]...), marker)
			if !reportContains(removed, section.name) {
				removed = append(removed, section.name)
			}
		}
	}
	text := rendered()
	if len(text) > budget {
		return "", fmt.Errorf("a message budget of %d bytes cannot hold this report even reduced to its required parts; raise the budget rather than shipping a message that lost them", budget)
	}
	return text, nil
}
func workRestoreLines(r map[string]any) []string {
	restoreLines := []string{}
	if pyvalue.Truthy(r["restore"]) {
		restore := py.Dict(r["restore"], false)
		restoreLines = []string{"", "workflow restore:"}
		for _, entry := range [][2]string{{"mode", "mode"}, {"scope", "scope"}, {"phase", "phase"}, {"phaseObservedAt", "phase observed"}, {"plan", "plan"}, {"evidence", "evidence"}, {"remaining", "remaining"}} {
			if value := reportValue(restore, entry[0]); pyvalue.Truthy(restore[entry[0]]) {
				restoreLines = append(restoreLines, "  "+entry[1]+": "+value)
			}
		}
		pointers := map[string]string{"development": "codexclaw:cxc-dev", "loop": "codexclaw:cxc-loop with codexclaw:cxc-pabcd", "lost-context": "codexclaw:cxc-lost-context", "pull-request": "codexclaw:cxc-dev references/stacked-prs.md", "review-repair": "codexclaw:cxc-review-repair"}
		for _, raw := range py.Items(restore["skills"]) {
			py.HashKey(raw)
			if pointer := pointers[reportString(raw)]; pointer != "" {
				restoreLines = append(restoreLines, "  read: "+pointer)
			} else {
				panic(&py.PythonError{Class: "KeyError", Detail: pyvalue.Repr(pyvalue.Repr(raw) + " has no recorded owner; name the owning skill rather than sending a recipient to reload everything")})
			}
		}
	}
	return restoreLines
}
func reportContains(values []string, word string) bool {
	for _, v := range values {
		if v == word {
			return true
		}
	}
	return false
}
