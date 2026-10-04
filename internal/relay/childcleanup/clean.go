// Package childcleanup releases finished child threads and their sub-threads so
// their MCP helpers stop. Automatic descendant-only cleanup retains the root;
// the subscription manager then unsubscribes it on the original connection.
// Archival restores visibility through thread/unarchive but cannot restore an
// interrupted turn. See docs/relay/subscriptions.md for guarantees and evidence.
package childcleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Host is the App Server connection a cleanup uses; *appserver.Client satisfies it.
type Host interface {
	Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error)
}

const (
	OutcomePlanned             = "planned"                // a dry run: this thread would be archived
	OutcomeArchived            = "archived"               // thread/archive accepted: unloaded, its MCP helpers stopped
	OutcomeReleasedByAncestor  = "released_by_ancestor"   // archive said "no rollout" and the thread is no longer loaded: an ancestor's archive took it
	OutcomeHeldActive          = "held_active"            // something in the subtree is running: nothing was archived
	OutcomeHeldIncomplete      = "held_incomplete"        // the subtree could not be established exactly: nothing was archived
	OutcomeNoRolloutLeftLoaded = "no_rollout_left_loaded" // never ran a turn, so archive refuses it: reported and left (deleting it cannot be proved lossless)
	OutcomeFailed              = "failed"                 // the host refused the archive for another reason
)

// The bounds of one discovery. Passing one is an error and nothing is archived, never a smaller cleanup.
var (
	maxLoadedPages = 100
	maxReads       = 2000
)

const noRollout = "no rollout found"

// Options of one cleanup.
type Options struct {
	DryRun          bool // plan only: nothing that changes the host is called
	DescendantsOnly bool // retain the root as a hold anchor, never archive it
	// Recheck runs once, after the subtree is known and before the first archive; an error stops the cleanup with nothing archived.
	Recheck func(context.Context) error
}

// Item is what happened to one loaded thread of the subtree.
type Item struct{ ThreadID, ParentID, Outcome, Detail string }

// Report is the answer of Clean. Items are in the order they were handled: deepest sub-threads first, the child last.
type Report struct {
	Child  string
	DryRun bool
	Items  []Item
	// Stopped is the error that ended the cleanup before every thread was handled; Items is what was done until then.
	Stopped string
	// Unresolved are loaded threads whose ancestry could not be read, or that sit on a parent cycle: any of them may belong to the subtree, so nothing is archived.
	Unresolved []string
}

// Complete is whether the subtree is released (for a dry run: would be): nothing held, left loaded or failed, and nothing unresolved.
func (r Report) Complete() bool {
	return r.Stopped == "" && len(r.Unresolved) == 0 && !slices.ContainsFunc(r.Items, func(i Item) bool {
		return slices.Contains([]string{OutcomeHeldActive, OutcomeHeldIncomplete, OutcomeNoRolloutLeftLoaded, OutcomeFailed}, i.Outcome)
	})
}

// Object is the report as the relay prints it.
func (r Report) Object() contract.OrderedObject {
	items, unresolved := make([]any, len(r.Items)), []any{}
	for i, it := range r.Items {
		var parent any
		if it.ParentID != "" {
			parent = it.ParentID
		}
		items[i] = contract.OrderedObject{{Key: "thread_id", Value: it.ThreadID}, {Key: "parent_thread_id", Value: parent}, {Key: "outcome", Value: it.Outcome}, {Key: "detail", Value: it.Detail}}
	}
	for _, id := range r.Unresolved {
		unresolved = append(unresolved, id)
	}
	return contract.OrderedObject{{Key: "child_thread_id", Value: r.Child}, {Key: "dry_run", Value: r.DryRun}, {Key: "complete", Value: r.Complete()}, {Key: "items", Value: items}, {Key: "unresolved", Value: unresolved}, {Key: "stopped", Value: optional(r.Stopped)}}
}

func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// member is a loaded thread of the child's subtree.
type member struct {
	id, parent string
	depth      int
}

type reading struct{ parent, status string }

// discovery is one reading of the App Server: scan resets its caches.
type discovery struct {
	host   Host
	cache  map[string]reading
	fail   map[string]bool
	reads  int
	strict bool // Automatic cleanup preserves transport failures for bounded retries.
}

// Clean releases the loaded threads of child's subtree, the child included when it is loaded: it reads thread/loaded/list, walks every loaded thread's parentThreadId up to the child and archives the
// members deepest first with the child last. Threads that are not loaded hold no helpers and are never touched. Any member that is running, and any loaded thread whose ancestry cannot be established,
// holds the whole subtree: nothing is archived. The subtree is read a second time and Recheck judges the relationship right after it, immediately before the first archive; the App Server has no fence
// against another client's turn starting after the final read. Unarchive restores visibility, not interrupted work. DescendantsOnly also requires idle/completed observations and never archives the root.
// A repeat finds nothing loaded and changes nothing. The caller decides whether the child may be cleaned up (Judge).
func Clean(ctx context.Context, host Host, child string, opts Options) (Report, error) {
	report := Report{Child: child, DryRun: opts.DryRun}
	d := &discovery{host: host, strict: opts.DescendantsOnly, cache: map[string]reading{}, fail: map[string]bool{}}
	stop := func(err error) (Report, error) {
		report.Stopped = err.Error()
		return report, err
	}
	if opts.DescendantsOnly {
		hold, detail, err := d.completedIdle(ctx, child)
		if err != nil {
			return stop(err)
		}
		if hold != "" {
			report.Items = append(report.Items, Item{ThreadID: child, Outcome: hold, Detail: detail})
			return report, nil
		}
	}
	members, hold, detail, err := d.scan(ctx, child, &report)
	if err == nil && hold == "" && !opts.DryRun && len(members) > 0 {
		if members, hold, detail, err = d.scan(ctx, child, &report); err == nil && hold == "" && opts.Recheck != nil {
			err = opts.Recheck(ctx)
		}
	}
	if err != nil {
		return stop(err)
	}
	if opts.DescendantsOnly && hold == "" {
		for _, m := range members {
			hold, detail, err = d.completedIdle(ctx, m.id)
			if err != nil {
				return stop(err)
			}
			if hold != "" {
				break
			}
		}
	}
	for _, m := range members {
		if opts.DescendantsOnly && hold == "" && m.depth > 0 {
			hold, detail, err = d.completedIdle(ctx, child)
			if err == nil && hold == "" {
				hold, detail, err = d.completedIdle(ctx, m.id)
			}
			if err != nil {
				return stop(err)
			}
		}
		if opts.DescendantsOnly && m.depth == 0 && hold == "" {
			continue
		}
		switch {
		case hold != "":
			report.Items = append(report.Items, Item{ThreadID: m.id, ParentID: m.parent, Outcome: hold, Detail: detail})
		case opts.DryRun:
			report.Items = append(report.Items, Item{ThreadID: m.id, ParentID: m.parent, Outcome: OutcomePlanned, Detail: "thread/archive"})
		default:
			outcome, detail, err := d.archive(ctx, m.id)
			if err != nil {
				return stop(err)
			}
			report.Items = append(report.Items, Item{ThreadID: m.id, ParentID: m.parent, Outcome: outcome, Detail: detail})
			if opts.DescendantsOnly && (outcome == OutcomeFailed || outcome == OutcomeNoRolloutLeftLoaded) {
				return stop(fmt.Errorf("descendant archive: %s: %s", m.id, detail))
			}
		}
	}
	// a never-run thread left loaded may have been taken by the archive of an ancestor that followed
	if ids, err := d.loadedIDs(ctx); err == nil && slices.ContainsFunc(report.Items, func(it Item) bool { return it.Outcome == OutcomeNoRolloutLeftLoaded }) {
		for i, it := range report.Items {
			if it.Outcome == OutcomeNoRolloutLeftLoaded && !slices.Contains(ids, it.ThreadID) {
				report.Items[i].Outcome, report.Items[i].Detail = OutcomeReleasedByAncestor, "no longer loaded after the archives that followed"
			}
		}
	}
	return report, nil
}

// scan reads the loaded threads afresh and finds the members of child's subtree, deepest first. hold names the outcome that keeps the whole subtree from being archived, if any.
func (d *discovery) scan(ctx context.Context, child string, report *Report) (members []member, hold, detail string, err error) {
	d.cache, d.fail, d.reads, report.Unresolved = map[string]reading{}, map[string]bool{}, 0, nil
	loaded, err := d.loadedIDs(ctx)
	if err != nil {
		return nil, "", "", err
	}
	for _, id := range loaded {
		switch m, found, resolved, err := d.locate(ctx, id, child); {
		case err != nil:
			return nil, "", "", err
		case !resolved:
			report.Unresolved = append(report.Unresolved, id)
		case found:
			members = append(members, m)
		}
	}
	slices.Sort(report.Unresolved)
	slices.SortFunc(members, func(a, b member) int {
		if a.depth != b.depth {
			return b.depth - a.depth
		}
		return strings.Compare(a.id, b.id)
	})
	if len(report.Unresolved) > 0 {
		hold, detail = OutcomeHeldIncomplete, "the ancestry of a loaded thread could not be established; nothing was archived"
	}
	for _, m := range members {
		if d.cache[m.id].status == "active" {
			hold, detail = OutcomeHeldActive, m.id+" in this subtree is running; nothing was archived"
		}
	}
	return members, hold, detail, nil
}

// archive asks the host to archive one thread and classifies the answer. Only the host's own refusal is a result for this thread: a transport or phase failure, or a cancellation, leaves this request's
// outcome unknown and ends the cleanup (an error). "no rollout found" is also what a thread an ancestor's archive already took answers, so the loaded set is read again first.
func (d *discovery) archive(ctx context.Context, id string) (outcome, detail string, err error) {
	_, err = d.host.Call(ctx, "thread/archive", map[string]any{"threadId": id})
	var refusal *appserver.RPCError
	switch {
	case err == nil:
		return OutcomeArchived, "", nil
	case !errors.As(err, &refusal):
		return "", "", err
	case !strings.Contains(err.Error(), noRollout):
		return OutcomeFailed, err.Error(), nil
	}
	ids, err := d.loadedIDs(ctx)
	switch {
	case err != nil:
		return "", "", err
	case !slices.Contains(ids, id):
		return OutcomeReleasedByAncestor, "no longer loaded", nil
	}
	return OutcomeNoRolloutLeftLoaded, "archive refused it (no rollout: it never ran a turn); it stays loaded until its owner, the connection that created or resumed its parent, closes or unsubscribes, about 60 s after that", nil
}

// loadedIDs is every id thread/loaded/list reports, all pages.
func (d *discovery) loadedIDs(ctx context.Context) ([]string, error) {
	var ids []string
	for page, cursor := 0, ""; ; page++ {
		if page >= maxLoadedPages {
			return nil, fmt.Errorf("thread/loaded/list has more than %d pages", maxLoadedPages)
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := d.host.Call(ctx, "thread/loaded/list", params)
		if err != nil {
			return nil, err
		}
		var out struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("thread/loaded/list answer: %w", err)
		}
		if ids = append(ids, out.Data...); out.NextCursor == nil || *out.NextCursor == "" {
			return ids, nil
		}
		cursor = *out.NextCursor
	}
}

// read is a thread's parentThreadId and status. It never asks for turns: includeTurns is unsupported on an ephemeral thread and materializes the rollout of a thread that never ran. A thread that cannot
// be read answers ok false; passing the read bound is an error.
func (d *discovery) read(ctx context.Context, id string) (r reading, ok bool, err error) {
	if r, ok = d.cache[id]; ok || d.fail[id] {
		return r, ok, nil
	}
	if d.reads++; d.reads > maxReads {
		return r, false, fmt.Errorf("more than %d thread/read calls", maxReads)
	}
	raw, err := d.host.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	hostErr := err
	var out struct {
		Thread struct {
			ID     string  `json:"id"`
			Parent *string `json:"parentThreadId"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	// an answer that does not name this thread or a status the host defines is not evidence of anything: the thread is unresolved
	if err == nil && (out.Thread.ID != id || !slices.Contains([]string{"idle", "active", "systemError", "notLoaded"}, out.Thread.Status.Type)) {
		err = errors.New("unusable thread/read answer")
	}
	if err != nil {
		d.fail[id] = true
		var refusal *appserver.RPCError
		if d.strict && hostErr != nil && !errors.As(hostErr, &refusal) {
			return r, false, hostErr
		}
		return r, false, ctx.Err()
	}
	r = reading{status: out.Thread.Status.Type}
	if out.Thread.Parent != nil {
		r.parent = *out.Thread.Parent
	}
	d.cache[id] = r
	return r, true, nil
}

// locate walks id's parent chain to see whether it reaches child. resolved is false when the chain cannot be read or loops.
func (d *discovery) locate(ctx context.Context, id, child string) (m member, found, resolved bool, err error) {
	first, ok, err := d.read(ctx, id)
	if err != nil || !ok {
		return m, false, false, err
	}
	seen := map[string]bool{}
	for cur, depth := id, 0; ; depth++ {
		if cur == child {
			return member{id: id, parent: first.parent, depth: depth}, true, true, nil
		}
		if seen[cur] {
			return m, false, false, nil
		}
		seen[cur] = true
		r, ok, err := d.read(ctx, cur)
		if err != nil || !ok {
			return m, false, false, err
		}
		if r.parent == "" {
			return m, false, true, nil
		}
		cur = r.parent
	}
}
