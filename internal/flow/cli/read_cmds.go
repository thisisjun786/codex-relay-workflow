package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

// ps is `crw flow ps`: one line for each run that is not over (all of them with --all).
func (c *command) ps(args []string) int {
	fs, cm := c.flags()
	all := fs.Bool("all", false, "list the runs that are over too")
	asJSON := fs.Bool("json", false, "print the runs as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 0 {
		return c.usage("takes no run id (crw flow show RUN reads one)")
	}
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err)
	}
	defer r.Close()
	infos, err := r.Infos(c.ctx)
	if err != nil {
		return c.fail("%v", err)
	}
	var shown []RunInfo
	for _, info := range infos {
		if *all || !Over(info.State) {
			shown = append(shown, info)
		}
	}
	if *asJSON {
		if shown == nil {
			shown = []RunInfo{}
		}
		_, _ = c.stdout.Write(JSON(struct {
			Runs []RunInfo `json:"runs"`
		}{shown}))
		return 0
	}
	if len(shown) == 0 {
		fmt.Fprintln(c.stdout, "no runs")
		return 0
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATE\tWORKFLOW\tOWNER\tCALLS\tOPEN\tHOLD\tASKS\tTOKENS\tCACHED\tFRESH\tOUTPUT\tSTARTED")
	for _, i := range shown {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", i.RunID, i.State, i.Workflow, short(i.Owner), i.Calls, i.Unsettled, i.Holds, i.Waiting, i.Tokens,
			i.TokenSplit.CachedInput, i.TokenSplit.FreshInput, i.TokenSplit.Output, c.since(i.StartedAt))
	}
	_ = tw.Flush()
	return 0
}

// show is `crw flow show RUN`: the tree phase, then agent, with the nodes' commits, reviews and tokens, and recovery_hold where a
// call's outcome is unknown.
func (c *command) show(args []string) int {
	fs, cm := c.flags()
	asJSON := fs.Bool("json", false, "print the run as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		return c.usage("exactly one run id is required")
	}
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err)
	}
	defer r.Close()
	d, err := r.Detail(c.ctx, positional[0])
	if err != nil {
		return c.fail("%v", err)
	}
	if *asJSON {
		_, _ = c.stdout.Write(JSON(d))
		return 0
	}
	c.printDetail(d)
	return 0
}

func (c *command) printDetail(d RunDetail) {
	w := c.stdout
	fmt.Fprintf(w, "run %s  %s  %s\n", d.RunID, d.Workflow, d.State)
	fmt.Fprintf(w, "owner %s  plan %s@%d  started %s ago  tokens %d (%s)  calls %d\n", cell(d.Owner), cell(d.PlanID), d.PlanRevision, c.since(d.StartedAt), d.Tokens, d.TokenSplit, d.Calls)
	switch {
	case d.PolicyPath == "" && d.PolicyUnrecorded && d.PolicyDigest != "":
		fmt.Fprintln(w, "policy file not recorded  digest "+d.PolicyDigest+" (this run started before the policy file was journaled)")
	case d.PolicyPath == "" && d.PolicyUnrecorded:
		fmt.Fprintln(w, "policy not recorded (this run started before the policy file was journaled)")
	case d.PolicyPath == "":
		fmt.Fprintln(w, "policy none (placeholder table, no execution policy file)")
	default:
		line := "policy " + d.PolicyPath + "  digest " + d.PolicyDigest
		if d.PolicyRepinnedFrom != "" {
			line += "  repinned from " + d.PolicyRepinnedFrom
		}
		fmt.Fprintln(w, line)
	}
	var notes []string
	if d.Live {
		notes = append(notes, fmt.Sprintf("process %d holds the run lock", d.PID))
	}
	if d.Paused != "" {
		notes = append(notes, "paused by "+d.Paused)
	}
	if len(d.HeldReasons) > 0 {
		notes = append(notes, "calls held for "+strings.Join(d.HeldReasons, ", "))
	}
	if d.Holds > 0 {
		notes = append(notes, fmt.Sprintf("%d call(s) in recovery_hold: crw flow recover", d.Holds))
	}
	if d.Unsettled-d.Holds > 0 && !d.Live {
		// while a process holds the run lock the run goes on by itself, and a resume is refused
		notes = append(notes, fmt.Sprintf("%d call(s) to go on with: crw flow resume %s", d.Unsettled-d.Holds, d.RunID))
	}
	if d.State == StateBlocked {
		notes = append(notes, "no node started: no crw.release released a node and nothing landed")
	}
	for _, ie := range d.ItemErrors {
		notes = append(notes, fmt.Sprintf("item %s failed: %s", cell(ie.Item), oneLine(ie.Error)))
	}
	if d.State == StateDead {
		notes = append(notes, "no process holds the run: crw flow resume "+d.RunID)
	}
	for _, n := range notes {
		fmt.Fprintf(w, "  %s\n", n)
	}
	for _, p := range d.Phases {
		fmt.Fprintf(w, "phase %s\n", cell(p.Title))
		for _, a := range p.Agents {
			line := fmt.Sprintf("  %-14s %-8s %-12s", cell(a.Role), cell(a.Node), a.Status)
			if a.Reason != "" {
				line += " (" + a.Reason + ")"
			}
			if a.ThreadID != "" {
				line += "  thread " + a.ThreadID
			}
			if a.Tokens > 0 {
				line += fmt.Sprintf("  tokens %d (%s)", a.Tokens, a.TokenSplit)
			}
			if a.RecoveryHold {
				line += "  [recovery_hold]"
			}
			fmt.Fprintln(w, line)
		}
	}
	for _, n := range d.Nodes {
		line := "node " + n.Node
		if len(n.Commits) > 0 {
			line += "  commits " + strings.Join(shorten(n.Commits), ",")
		}
		for _, rv := range n.Reviews {
			line += fmt.Sprintf("  review %s %s@%s", rv.Call, cell(rv.Verdict+" "+rv.State), short(rv.Head))
		}
		if n.Accepted != "" {
			line += "  accepted " + n.Accepted
		}
		if n.Integrated != "" {
			line += "  integrated " + short(n.Integrated)
		}
		if n.Tokens > 0 {
			line += fmt.Sprintf("  tokens %d", n.Tokens)
		}
		if n.RecoveryHold {
			line += "  [recovery_hold]"
		}
		fmt.Fprintln(w, line)
	}
	for _, q := range d.Questions {
		state := "waiting"
		switch {
		case q.Applied != "":
			state = "answered by " + q.Applied
		case !q.Current:
			state = "replaced by a later question"
		}
		fmt.Fprintf(w, "question %s r%d  node %s  %s  %s\n", q.ID, q.Revision, cell(q.Node), q.Reason, state)
	}
}

func shorten(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = short(s)
	}
	return out
}

// watch is `crw flow watch RUN`: the tree of the run, then its journal read as `crw relay journal-tail` reads the relay journal.
// Without --after it prints the tree as of the last row it read (the same phase, then agent tree as `show`, recovery_hold included)
// and the cursor to pass back. With --after it prints the rows after that cursor, at most --limit, waiting up to --wait seconds when
// there are none, and the new cursor. --follow keeps reading from the cursor until the run is over or has no process, printing each
// row as it arrives and the tree again at the end.
func (c *command) watch(args []string) int {
	fs, cm := c.flags()
	after := fs.Int("after", 0, "the seq of the last row already seen: print the rows after it (without it, print the tree)")
	wait := fs.Int("wait", 0, "seconds to wait for a row when there is none after the cursor (0 to 3600)")
	limit := fs.Int("limit", 100, "at most this many rows in an answer (1 to 1000)")
	follow := fs.Bool("follow", false, "keep reading from the cursor until the run is over")
	asJSON := fs.Bool("json", false, "print each answer as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	cursorGiven := false
	fs.Visit(func(f *flag.Flag) { cursorGiven = cursorGiven || f.Name == "after" })
	switch {
	case len(positional) != 1:
		return c.usage("exactly one run id is required")
	case *after < 0:
		return c.usage("--after is a journal seq, 0 or more")
	case *limit < 1 || *limit > 1000:
		return c.usage("--limit is between 1 and 1000")
	case *wait < 0 || *wait > 3600:
		return c.usage("--wait is between 0 and 3600 seconds")
	}
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err)
	}
	defer r.Close()
	run := positional[0]
	cursor := *after
	if !cursorGiven {
		// the tree first: its cursor is the last row it was made of, so the rows that follow are exactly what it lacks
		d, err := r.Detail(c.ctx, run)
		if err != nil {
			return c.fail("%v", err)
		}
		cursor = d.LastSeq
		if *asJSON {
			b, _ := json.Marshal(struct {
				Run    string    `json:"run"`
				Cursor int       `json:"cursor"`
				State  string    `json:"state"`
				Tree   RunDetail `json:"tree"`
			}{run, cursor, d.State, d})
			fmt.Fprintf(c.stdout, "%s\n", b)
		} else {
			c.printDetail(d)
			fmt.Fprintf(c.stdout, "cursor %d  state %s\n", cursor, d.State)
		}
		if !*follow {
			return 0
		}
	}
	for {
		w := time.Duration(*wait) * time.Second
		if *follow && w == 0 {
			w = 30 * time.Second
		}
		page, err := r.Tail(c.ctx, run, cursor, *limit, w)
		if err != nil {
			if c.ctx.Err() != nil {
				return 0
			}
			return c.fail("%v", err)
		}
		if *asJSON {
			b, _ := json.Marshal(page)
			fmt.Fprintf(c.stdout, "%s\n", b)
		} else {
			for _, row := range page.Rows {
				fmt.Fprintln(c.stdout, rowLine(row))
			}
			if !*follow {
				fmt.Fprintf(c.stdout, "cursor %d  more %t  state %s\n", page.Cursor, page.More, page.State)
			}
		}
		cursor = page.Cursor
		if !*follow {
			return 0
		}
		if len(page.Rows) == 0 && !page.More && (Over(page.State) || page.State == StateDead || page.State == StateHeld) {
			if !*asJSON {
				if d, err := r.Detail(c.ctx, run); err == nil {
					c.printDetail(d)
				}
			}
			return 0
		}
	}
}

// rowLine is one journal row on one line.
func rowLine(row Row) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%6d", row.Seq), row.Kind+"/"+row.Event)
	if row.CallID != "" {
		parts = append(parts, short(row.CallID))
	}
	if row.Label != "" {
		parts = append(parts, row.Label)
	}
	for _, key := range []string{"title", "reason", "status", "node", "thread_id"} {
		if v, ok := row.Data[key].(string); ok && v != "" {
			parts = append(parts, key+"="+oneLine(v))
		}
	}
	return strings.Join(parts, "  ")
}
