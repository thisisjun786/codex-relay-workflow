package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// Row 7 (resumable Codex threads) rests on one measured host rule, codex-cli 0.154.0 in an
// isolated Codex home on 2026-09-30 (docs/plugin-packaging.md "The turn-command cache"): a
// plugin's hook command is resolved from the version installed when a turn starts, with
// ${PLUGIN_ROOT} expanded, and held for that turn only. The next turn of the same thread - still
// loaded in the same App Server, resumed there or in another process - and every new thread
// resolve the version installed then. So a thread can still run a hook command from a replaced
// version only while a turn that started before the replacement has not ended.

// DefaultSocket is the App Server socket row 7 asks when none is given: the one `crw bridge`
// defaults to under the Codex home, which is the socket the relay's service is run against.
func DefaultSocket(codexHome string) string {
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
}

// threadSourceKinds is every thread source codex-cli 0.154.0 records (ThreadSourceKind).
// thread/list defaults to the interactive ones, and a subagent or exec thread runs turns too.
var threadSourceKinds = []any{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}

// threadPage is the thread/list page size, the most `crw bridge`'s list_threads asks for.
const threadPage = 100

// cachedVersion is one cached plugin version directory as row 5 read it.
type cachedVersion struct {
	dir string
	// installed is when the version was installed: the latest modification or status-change
	// time of its directory (the entry and what it leads to). `codex plugin add` writes a new
	// directory and does not keep the source's times, so none of these predates the install.
	installed time.Time
	dated     bool
	// python: row 5 reports one of its hook commands as Python. unjudged: row 5 could not read or
	// judge every hook command it declares.
	python, unjudged bool
}

// installedAt is the latest mtime or ctime of path and of what it leads to.
func installedAt(path string) (time.Time, error) {
	var latest time.Time
	for _, stat := range []func(string, *unix.Stat_t) error{unix.Lstat, unix.Stat} {
		var st unix.Stat_t
		if err := stat(path, &st); err != nil {
			return time.Time{}, err
		}
		for _, ts := range []unix.Timespec{st.Mtim, st.Ctim} {
			if at := time.Unix(ts.Unix()); at.After(latest) {
				latest = at
			}
		}
	}
	return latest, nil
}

// listedThread and listedTurn are the fields of the App Server's Thread and Turn row 7 reads.
type listedThread struct {
	ID     string  `json:"id"`
	Name   *string `json:"name"`
	Status struct {
		Type string `json:"type"`
	} `json:"status"`
	CreatedAt *int64 `json:"createdAt"`
	UpdatedAt *int64 `json:"updatedAt"`
}

type listedTurn struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	StartedAt   *int64 `json:"startedAt"`
	CompletedAt *int64 `json:"completedAt"`
}

type page struct {
	Data       *[]json.RawMessage `json:"data"`
	NextCursor *string            `json:"nextCursor"`
}

func seconds(v *int64) any {
	if v == nil {
		return nil
	}
	return time.Unix(*v, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func nullableText(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// sameDirectory reports whether two paths name one directory, links resolved where they can be.
func sameDirectory(a, b string) bool {
	resolve := func(p string) string {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return resolved
		}
		return filepath.Clean(p)
	}
	return a != "" && b != "" && resolve(a) == resolve(b)
}

// threads is row 7: every thread the App Server at the socket lists, archived or not, read-only
// (thread/list with pagination, the way `crw bridge`'s list_threads asks, and thread/turns/list
// for each thread's newest turn). A thread is a reference when its newest turn may still be
// running and was given its hook commands by a version this scan cannot clear: one installed
// no longer (no cached version was installed before the turn started) or one row 5 reports as
// Python or could not judge. A turn may still be running when it is inProgress, or
// interrupted with no completedAt, which is how a turn running in another App Server process,
// or one whose process died, reads (measured on codex-cli 0.154.0). An App Server that cannot be
// reached, or that serves another Codex home, leaves the row unscanned: nothing was read.
func (s *scan) threads(ctx context.Context) {
	socket := s.o.Socket
	if !s.cacheListed {
		s.surface(7, false, 0, "the plugin cache could not be listed (row 5), so no turn can be placed against the version it was given")
		return
	}
	client := appserver.New(socket, appserver.DefaultBounds)
	defer func() { _ = client.Close() }()
	establish, cancel := context.WithTimeout(ctx, appserver.DefaultBounds.Establish)
	err := client.Connect(establish)
	cancel()
	if err != nil {
		s.surface(7, false, 0, "the App Server at "+socket+" could not be reached ("+err.Error()+"), so which threads may still run a hook command from a replaced plugin version is unknown")
		return
	}
	served, _ := client.Info()["codexHome"].(string)
	if !sameDirectory(served, s.o.CodexHome) {
		s.surface(7, false, 0, "the App Server at "+socket+" serves the Codex home "+strconv.Quote(served)+", not "+s.o.CodexHome+" (initialize codexHome), so its threads are not the ones this plugin cache serves")
		return
	}
	examined, running := 0, 0
	for _, archived := range []bool{false, true} {
		for _, thread := range s.listThreads(ctx, client, socket, archived) {
			examined++
			if s.thread(ctx, client, socket, thread, archived) {
				running++
			}
		}
	}
	s.surface(7, true, examined, strconv.Itoa(examined)+" threads the App Server at "+socket+" lists for "+s.o.CodexHome+" (every source kind, archived included), "+strconv.Itoa(running)+
		" with a newest turn that may still be running (inProgress, or interrupted with no completedAt). A hook command is resolved when a turn starts and held for that turn only (codex-cli 0.154.0), so such a turn is a reference when no cached version was installed before it started (its startedAt, in seconds, against the latest mtime or ctime of each version directory), or when the version installed last before it started is one whose hook commands row 5 reports as Python or could not judge")
}

// listThreads pages through thread/list for archived or not. A page that cannot be read or
// decoded, a malformed thread and a cursor that repeats are unreadable: the threads after them
// are unknown.
func (s *scan) listThreads(ctx context.Context, client *appserver.Client, socket string, archived bool) []listedThread {
	var out []listedThread
	var cursor *string
	seen := map[string]bool{}
	which := "thread/list (archived " + strconv.FormatBool(archived) + ")"
	for {
		params := map[string]any{"limit": threadPage, "useStateDbOnly": true, "sourceKinds": threadSourceKinds, "archived": archived}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		raw, err := client.Call(ctx, "thread/list", params)
		var p page
		if err == nil {
			err = json.Unmarshal(raw, &p)
		}
		if err == nil && p.Data == nil {
			err = errors.New("the answer has no data")
		}
		if err != nil {
			s.unreadable = append(s.unreadable, socket+": row 7 "+which+": "+err.Error()+", so the threads from this page on are unknown")
			return out
		}
		for _, entry := range *p.Data {
			var t listedThread
			if err := json.Unmarshal(entry, &t); err != nil || t.ID == "" {
				s.unreadable = append(s.unreadable, socket+": row 7 "+which+": a thread entry this scan cannot read ("+clip(string(entry))+"), so whether it may still run a hook command is unknown")
				continue
			}
			out = append(out, t)
		}
		if p.NextCursor == nil {
			return out
		}
		if seen[*p.NextCursor] {
			s.unreadable = append(s.unreadable, socket+": row 7 "+which+": the cursor "+strconv.Quote(*p.NextCursor)+" came back again, so the listing does not end and the threads after it are unknown")
			return out
		}
		seen[*p.NextCursor] = true
		cursor = p.NextCursor
	}
}

func clip(text string) string {
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

// thread judges one listed thread by its newest turn, and reports whether that turn may still
// be running.
func (s *scan) thread(ctx context.Context, client *appserver.Client, socket string, t listedThread, archived bool) bool {
	where := socket + ": row 7 thread " + t.ID
	raw, err := client.Call(ctx, "thread/turns/list", map[string]any{"threadId": t.ID, "limit": 1, "sortDirection": "desc", "itemsView": "summary"})
	var p page
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err == nil && p.Data == nil {
		err = errors.New("the answer has no data")
	}
	if err != nil {
		s.unreadable = append(s.unreadable, where+": its newest turn could not be read ("+err.Error()+"), so whether it may still run a hook command is unknown")
		return false
	}
	if len(*p.Data) == 0 {
		return false // no turn: nothing holds a hook command
	}
	var turn listedTurn
	if err := json.Unmarshal((*p.Data)[0], &turn); err != nil || turn.ID == "" {
		s.unreadable = append(s.unreadable, where+": its newest turn cannot be read ("+clip(string((*p.Data)[0]))+"), so whether it may still run a hook command is unknown")
		return false
	}
	switch {
	case turn.Status == "completed" || turn.Status == "failed" || (turn.Status == "interrupted" && turn.CompletedAt != nil):
		return false // ended: the thread's next turn resolves the version installed then
	case turn.Status != "inProgress" && turn.Status != "interrupted":
		s.unreadable = append(s.unreadable, where+": its newest turn "+turn.ID+" has the status "+strconv.Quote(turn.Status)+", which this scan does not know, so whether it may still run a hook command is unknown")
		return false
	}
	root := filepath.Join(s.o.CodexHome, "plugins", "cache", "crw", "crw")
	e := Executable{Value: root, Kind: KindMissing, Python: true}
	var version cachedVersion
	var found bool
	undated := false
	for _, v := range s.versions {
		undated = undated || !v.dated
	}
	running := "its newest turn " + turn.ID + " is " + turn.Status
	if turn.Status == "interrupted" {
		running += " with no completedAt, which is how a turn still running in another App Server process reads, and a turn whose process died (settled once a turn on the thread runs to completion)"
	} else {
		running += " in this App Server"
	}
	switch {
	case turn.StartedAt == nil:
		e.Kind, e.Detail = KindUnreadable, running+", and it records no startedAt, so the plugin version that gave it its hook commands cannot be placed; counted as Python"
	case undated:
		e.Kind, e.Detail = KindUnreadable, running+", and the install time of a cached plugin version could not be read (row 5), so the version that gave it its hook commands cannot be placed; counted as Python"
	default:
		started := time.Unix(*turn.StartedAt, 0)
		version, found = s.inForce(started)
		switch {
		case !found && len(s.versions) == 0:
			e.Detail = running + ", and no plugin version is cached, so the version that gave it its hook commands at " + seconds(turn.StartedAt).(string) + " has been removed and cannot be read; counted as Python"
		case !found:
			e.Detail = running + "; it started at " + seconds(turn.StartedAt).(string) + ", and no cached version was installed before it (the earliest at " + s.earliest().UTC().Format(time.RFC3339Nano) + "), so it was given its hook commands by a version replaced since, which cannot be read; counted as Python"
		case version.python || version.unjudged:
			e.Value, e.Resolves, e.Kind = version.dir, version.dir, KindDirectory
			judged := "reports one of its hook commands as Python"
			if !version.python {
				judged = "could not judge every hook command it declares (see unreadable); counted as Python"
			}
			e.Detail = running + "; it started at " + seconds(turn.StartedAt).(string) + ", after " + version.dir + " was installed (" + version.installed.UTC().Format(time.RFC3339Nano) + "), whose declarations it was given, and row 5 " + judged
		default:
			return true // given the hook commands of a version row 5 clears
		}
	}
	fields := record.Object{
		{Key: "threadId", Value: t.ID},
		{Key: "name", Value: nullableText(t.Name)},
		{Key: "threadStatus", Value: t.Status.Type},
		{Key: "archived", Value: archived},
		{Key: "threadCreatedAt", Value: seconds(t.CreatedAt)},
		{Key: "threadUpdatedAt", Value: seconds(t.UpdatedAt)},
		{Key: "turnId", Value: turn.ID},
		{Key: "turnStatus", Value: turn.Status},
		{Key: "turnStartedAt", Value: seconds(turn.StartedAt)},
		{Key: "turnCompletedAt", Value: seconds(turn.CompletedAt)},
		{Key: "versionDirectory", Value: nil},
		{Key: "versionInstalledAt", Value: nil},
	}
	if found {
		fields = record.Set(fields, "versionDirectory", version.dir)
		fields = record.Set(fields, "versionInstalledAt", version.installed.UTC().Format(time.RFC3339Nano))
	}
	s.reference(7, socket, "newestTurn", e, fields)
	return true
}

// inForce is the cached version a turn that started within the second beginning at started was
// given: the latest one installed before that second began. None when every cached version was
// installed at or after it.
func (s *scan) inForce(started time.Time) (cachedVersion, bool) {
	var best cachedVersion
	found := false
	for _, v := range s.versions {
		if v.installed.Before(started) && (!found || v.installed.After(best.installed)) {
			best, found = v, true
		}
	}
	return best, found
}

// earliest is the earliest install time of a cached version.
func (s *scan) earliest() time.Time {
	var first time.Time
	for i, v := range s.versions {
		if i == 0 || v.installed.Before(first) {
			first = v.installed
		}
	}
	return first
}
