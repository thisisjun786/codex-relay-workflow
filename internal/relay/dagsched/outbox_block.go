package dagsched

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// The text of a summary and of the block and container it is written in (docs/relay/dag-outbox.md). The relay never writes Linear: it states exactly what the parent writes and reads a
// readback by parsing these markers. They are HTML comments around a fenced body, the grammar the relationship outbox already relies on (the connector keeps comments and fenced text
// byte for byte); a plan's summary lives in one container per plan, which holds one block.

const (
	blockMarkerPrefix     = "<!-- relay-dag-summary:"
	blockEndPrefix        = "<!-- /relay-dag-summary:"
	containerMarkerPrefix = "<!-- relay-dag-summary-container:"
	containerEndPrefix    = "<!-- /relay-dag-summary-container:"
	markerSuffix          = " -->"
	// BlockFormat is the declared format of a block, the first header after the start marker.
	BlockFormat = "dag-summary/1"
)

func blockStart(id string) string       { return blockMarkerPrefix + id + markerSuffix }
func blockEnd(id string) string         { return blockEndPrefix + id + markerSuffix }
func containerStart(plan string) string { return containerMarkerPrefix + plan + markerSuffix }
func containerEnd(plan string) string   { return containerEndPrefix + plan + markerSuffix }

// canonText is a text as a connector may hand it back: line endings are \n, no line ends in a blank, and the text neither starts nor ends with a line break. A document is compared
// only in this form, so a connector that normalises line endings or trailing blanks does not make a written block read as another one.
func canonText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fenceFor is a backtick fence longer than any run of backticks in the text, so the text cannot close it.
func fenceFor(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// oneLine is a value printed on one line of the text: its blanks collapse and no line break survives.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// SummaryText is the text of a summary: the progress it states, in a fixed order and with no clock, so equal progress is equal text. It names the plan and the revision, the counts of accepted
// and integrated nodes against the denominator, the blocked nodes with their closed reasons, and every live node with its stage, its reason and its pull request.
func SummaryText(p Progress) string {
	var b strings.Builder
	state := "active"
	if p.PlanState != "" {
		state = p.PlanState
	}
	fmt.Fprintf(&b, "Plan %s, project %s, revision %d, %s\n", p.Reading.PlanID, p.ProjectKey, p.Reading.PlanRevision, state)
	fmt.Fprintf(&b, "Nodes: %d, accepted %d of %d, integrated %d of %d\n", p.Denominator.Nodes, p.Cumulative.Accepted.Nodes, p.Cumulative.Accepted.Of, p.Cumulative.Integrated.Nodes, p.Cumulative.Integrated.Of)
	changed := "unchanged"
	if p.Denominator.Changed {
		changed = "changed"
	}
	fmt.Fprintf(&b, "Denominator: %d at revision %d (%s; last changed at revision %d)\n", p.Denominator.Nodes, p.Denominator.Revision, changed, p.Denominator.LastChangedRevision)
	if p.Outside.Nodes > 0 {
		fmt.Fprintf(&b, "Outside the denominator: %d (with an acceptance %d, holding a slot %d)\n", p.Outside.Nodes, p.Outside.WithAcceptance, p.Outside.HoldingSlot)
	}
	b.WriteString("\nStages\n")
	for _, st := range p.Stages {
		if st.Nodes > 0 {
			fmt.Fprintf(&b, "  %s %d: %s\n", st.Stage, st.Nodes, strings.Join(st.NodeIDs, ", "))
		}
	}
	if p.Blocked.Nodes > 0 {
		fmt.Fprintf(&b, "\nBlocked %d\n", p.Blocked.Nodes)
		for _, e := range p.Blocked.Entries {
			fmt.Fprintf(&b, "  %s, stage %s, %s", e.NodeID, e.Stage, e.Reason)
			if e.Detail != "" {
				fmt.Fprintf(&b, ": %s", oneLine(e.Detail))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nNodes\n")
	for _, n := range p.Nodes {
		fmt.Fprintf(&b, "  %s, %s, %s, stage %s", n.NodeID, n.IssueKey, n.Kind, n.Stage)
		if n.Reason != "" {
			fmt.Fprintf(&b, ", %s", n.Reason)
		}
		if n.Lifecycle != "" {
			fmt.Fprintf(&b, ", %s by the plan", n.Lifecycle)
		}
		if pr := n.Links.PullRequest; pr != nil {
			fmt.Fprintf(&b, ", pull request %s#%d", pr.Repository, pr.Number)
			if pr.HeadSHA != "" {
				fmt.Fprintf(&b, " at %s", pr.HeadSHA[:min(len(pr.HeadSHA), 12)])
			}
		}
		if n.Title != "" {
			fmt.Fprintf(&b, ", %s", oneLine(n.Title))
		}
		b.WriteString("\n")
	}
	return canonText(b.String())
}

// summaryBlock is the block of an entry: the markers, the headers that identify it (read back and compared one by one, never searched for as words) and the summary in a fence.
func summaryBlock(e SummaryEntry) string {
	body := canonText(e.Summary)
	fence := fenceFor(body)
	lines := []string{
		blockStart(e.SummaryID),
		"blockFormat: " + BlockFormat,
		"summaryId: " + e.SummaryID,
		"planId: " + e.PlanID,
		"projectKey: " + e.ProjectKey,
		"document: " + e.Document,
		"planRevision: " + strconv.FormatInt(e.PlanRevision, 10),
		"seq: " + strconv.FormatInt(e.Seq, 10),
		"stateDigest: " + e.StateDigest,
		"subjectDigest: " + e.SubjectDigest,
		"summarySha256: " + sha(body),
		"",
		fence + "text",
	}
	lines = append(lines, strings.Split(body, "\n")...)
	lines = append(lines, fence, blockEnd(e.SummaryID))
	return strings.Join(lines, "\n")
}

func summaryContainer(e SummaryEntry) string {
	return containerStart(e.PlanID) + "\n" + summaryBlock(e) + "\n" + containerEnd(e.PlanID)
}

func emptyContainer(plan string) string { return containerStart(plan) + "\n" + containerEnd(plan) }

// scannedBlock is one block found in a document.
type scannedBlock struct {
	id        string
	text      string // the block from its start marker to its end marker, canonical
	headers   map[string]string
	line      int // the line of its start marker
	container int // the container it lies in (0-based), -1 for none
}

// scannedDocument is what a document holds of one plan's summaries.
type scannedDocument struct {
	blocks     []scannedBlock // the blocks of this plan, in document order
	containers [][2]int       // the line ranges of the plan's containers
	containerN int            // how many container start markers there are
	problems   []string       // a block or container that is not closed or not paired
	lines      []string
}

// scanSummaryDocument reads a document for the blocks and containers of one plan. A block of another plan is not this plan's business (it has its own container), so only blocks whose
// planId header names this plan count; a block whose start marker has no end marker is a problem whichever plan it names, since what landed is then uncertain.
func scanSummaryDocument(doc, plan string) scannedDocument {
	d := scannedDocument{lines: strings.Split(canonText(doc), "\n")}
	var starts, ends []int
	for i, line := range d.lines {
		switch trimmed := strings.TrimSpace(line); trimmed {
		case containerStart(plan):
			starts = append(starts, i)
		case containerEnd(plan):
			ends = append(ends, i)
		}
	}
	d.containerN = len(starts)
	if len(starts) != len(ends) {
		d.problems = append(d.problems, "the plan's container is not closed (or is closed more often than it is opened)")
	} else {
		for k := range starts {
			if starts[k] >= ends[k] || (k+1 < len(starts) && ends[k] >= starts[k+1]) {
				d.problems = append(d.problems, "the plan's container markers are out of order")
				break
			}
			d.containers = append(d.containers, [2]int{starts[k], ends[k]})
		}
	}
	for i := 0; i < len(d.lines); i++ {
		trimmed := strings.TrimSpace(d.lines[i])
		if !strings.HasPrefix(trimmed, blockMarkerPrefix) || !strings.HasSuffix(trimmed, markerSuffix) {
			continue
		}
		id := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, blockMarkerPrefix), markerSuffix))
		headers := map[string]string{}
		end := -1
		for j := i + 1; j < len(d.lines); j++ {
			if strings.TrimSpace(d.lines[j]) == blockEnd(id) {
				end = j
				break
			}
		}
		// the headers: the lines after the start marker up to the first blank line
		for j := i + 1; j < len(d.lines) && strings.TrimSpace(d.lines[j]) != "" && (end < 0 || j < end); j++ {
			if key, value, ok := strings.Cut(d.lines[j], ": "); ok && !strings.Contains(key, " ") {
				headers[key] = value
			}
		}
		if end < 0 {
			if planOfBlock, named := headers["planId"]; !named || planOfBlock == plan {
				d.problems = append(d.problems, "the block of "+id+" is not closed, so what landed is uncertain")
			}
			continue
		}
		if headers["planId"] != plan {
			i = end
			continue
		}
		b := scannedBlock{id: id, text: strings.Join(d.lines[i:end+1], "\n"), headers: headers, line: i, container: -1}
		for k, c := range d.containers {
			if i > c[0] && end < c[1] {
				b.container = k
			}
		}
		d.blocks = append(d.blocks, b)
		i = end
	}
	return d
}

// containerBody is what a container holds besides its markers, canonical.
func (d scannedDocument) containerBody(k int) string {
	c := d.containers[k]
	return canonText(strings.Join(d.lines[c[0]+1:c[1]], "\n"))
}
