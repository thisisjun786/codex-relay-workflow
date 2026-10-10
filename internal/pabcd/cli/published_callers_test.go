package cli

// CRW-823: a session-state or goalplan write that published at the final path and then failed
// the post-rename directory sync is a written state. The oracle never syncs the published file
// or its directory (known-defects.md, "Found by CRW-479 state publication durability"), so it
// cannot fail after the rename and has no recorded corpus case here; these cases drive the Go
// port's own durability seam, which is a function argument the caller passes, never a
// package-level variable.
//
// The seam publishes for real and then reports the post-rename failure exactly as
// state.writeState does: a *state.PublishedError wrapping syscall.EIO, so state.Published(err)
// is true while errors.Is(err, syscall.EIO) still reaches the cause.
//
// HOME, CODEX_HOME and CRW_HOME point into temporary directories in every case (the review-round
// open packet probes CODEX_HOME), and those directories are checked afterwards, so a run that writes
// into them is reported instead of cleaned up (the real ~/.codex and ~/.crw are not observed, CRW-1170).
import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// cliPublishedIsolatedHome repoints HOME, CODEX_HOME and CRW_HOME into temporary directories for this test and
// fails it if the run created or removed anything at the top level of them. The real ~/.codex and ~/.crw are
// not observed: the host's Codex sessions write there at any moment (CRW-1170). It must be called before any
// helper that reads a home.
func cliPublishedIsolatedHome(t *testing.T) {
	t.Helper()
	testsupport.SandboxAccountHomes(t)
}

// cliPublishedStateWrite is the state seam: it publishes through the real writer and then reports
// the post-rename directory sync failure the way writeState does.
func cliPublishedStateWrite() func(string, state.State) error {
	return func(cwd string, s state.State) error {
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return &state.PublishedError{Err: syscall.EIO}
	}
}

// cliPublishedPlainWrite fails before publication, so nothing was published.
func cliPublishedPlainWrite() func(string, state.State) error {
	return func(string, state.State) error { return syscall.EIO }
}

// cliPublishedWrappedStateWrite publishes through the real writer and returns a PublishedError
// wrapped in another error, the way a caller that adds context around the write failure would.
// state.Published uses errors.As, so this must count as written exactly like the bare error.
func cliPublishedWrappedStateWrite() func(string, state.State) error {
	return func(cwd string, s state.State) error {
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return fmt.Errorf("recording the state: %w", &state.PublishedError{Err: syscall.EIO})
	}
}

// cliPublishedGoalplanWrite is the goalplan seam: it publishes through the real writer and then
// reports the post-rename failure the way writePublishAt does once it wraps it.
func cliPublishedGoalplanWrite() func(string, *goalplan.Goalplan) error {
	return func(cwd string, plan *goalplan.Goalplan) error {
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			return err
		}
		return &state.PublishedError{Err: syscall.EIO}
	}
}

func cliPublishedPlainGoalplanWrite() func(string, *goalplan.Goalplan) error {
	return func(string, *goalplan.Goalplan) error { return syscall.EIO }
}

func cliPublishedWantStateWarning() string {
	return "session state was published but its directory could not be synced: " + syscall.EIO.Error()
}

func cliPublishedWantGoalplanWarning(slug string) string {
	return "goalplan '" + slug + "' was published but its directory could not be synced: " + syscall.EIO.Error()
}

// The issue's first case: allow-write counts a published grant as recorded, answers its existing
// success with the warning appended and leaves exactly one grant. On dev the write error is
// reported as a plain failure.
func TestPublishedCallersMemoryAllowWriteReportsThePublishedGrant(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd, _, _ := cliSeed(t)
	out, code := cliPublishedMemoryAllowWrite(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd}, cliPublishedStateWrite())
	if code != 0 {
		t.Fatalf("code = %d, output %q", code, out)
	}
	if !strings.HasPrefix(out, "memory allow-write: session rec-s1 may perform ONE memory write; grant recorded for cwd "+cwd+";") {
		t.Errorf("the existing success line changed: %q", out)
	}
	if !strings.HasSuffix(out, "\n"+cliPublishedWantStateWarning()) {
		t.Errorf("output = %q, want the appended warning %q", out, cliPublishedWantStateWarning())
	}
	if s := state.ReadState(cwd, "rec-s1"); !s.MemoryWriteGrant {
		t.Errorf("the published grant is not visible: %+v", s)
	}
}

// A failure before publication is unchanged: the existing failure text and code, and no grant.
func TestPublishedCallersMemoryAllowWriteKeepsPrePublicationFailure(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd, _, _ := cliSeed(t)
	out, code := cliPublishedMemoryAllowWrite(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd}, cliPublishedPlainWrite())
	if code != 1 || !strings.HasPrefix(out, "memory allow-write: could not record the grant (") {
		t.Fatalf("got %d %q", code, out)
	}
	if strings.Contains(out, "published but") {
		t.Errorf("a pre-publication failure carries the warning: %q", out)
	}
}

// The issue's second case: scan record counts a published round as recorded, answers its existing
// success with the warning appended, and writes exactly one round and one ledger row. On dev the
// write error is reported as a failure and a retry appends a second row.
func TestPublishedCallersScanRecordReportsThePublishedRound(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd := scanRecordWorkspace(t)
	a := *ParseScanCliArgs([]string{"record", "--session", "s1", "--known", "goal=fact"}, cwd).Args
	res := cliPublishedScanRecordRun(a, state.AppendInterviewEvent, cliPublishedStateWrite())
	if res.Code != 0 {
		t.Fatalf("code = %d, output %q", res.Code, res.Output)
	}
	if !strings.HasPrefix(res.Output, "scan record: round 1 recorded for session s1 (contradictions=0, high=0") {
		t.Errorf("the existing success line changed: %q", res.Output)
	}
	if !strings.HasSuffix(res.Output, "\n"+cliPublishedWantStateWarning()) {
		t.Errorf("output = %q, want the appended warning %q", res.Output, cliPublishedWantStateWarning())
	}
	s := state.ReadState(cwd, "s1")
	if s.Interview == nil || s.Interview.ScanRounds != 1 || s.Interview.LastScanRoundID != 1 {
		t.Fatalf("the published round is not visible: %+v", s.Interview)
	}
	if got := len(state.ReadInterviewEvents(cwd, "s1")); got != 1 {
		t.Errorf("interview ledger rows = %d, want exactly 1 (a retry would duplicate the round)", got)
	}
}

func TestPublishedCallersScanRecordKeepsPrePublicationFailure(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd := scanRecordWorkspace(t)
	a := *ParseScanCliArgs([]string{"record", "--session", "s1"}, cwd).Args
	res := cliPublishedScanRecordRun(a, state.AppendInterviewEvent, cliPublishedPlainWrite())
	if res.Code != 1 || !strings.HasPrefix(res.Output, "scan record failed: ") {
		t.Fatalf("got %+v", res)
	}
	if strings.Contains(res.Output, "published but") {
		t.Errorf("a pre-publication failure carries the warning: %q", res.Output)
	}
	if s := state.ReadState(cwd, "s1"); s.Interview != nil && s.Interview.ScanRounds != 0 {
		t.Errorf("a pre-publication failure recorded a round: %+v", s.Interview)
	}
}

// state.Published reaches the publication through a wrapper: a caller that adds context with
// fmt.Errorf %w around the PublishedError still counts as written.
func TestPublishedCallersMemoryCountsAWrappedPublishedError(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd, _, _ := cliSeed(t)
	out, code := cliPublishedMemoryAllowWrite(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd}, cliPublishedWrappedStateWrite())
	if code != 0 || !strings.HasSuffix(out, "\n"+"session state was published but its directory could not be synced: recording the state: "+syscall.EIO.Error()) {
		t.Fatalf("a wrapped PublishedError was not read as published: %d %q", code, out)
	}
	if s := state.ReadState(cwd, "rec-s1"); !s.MemoryWriteGrant {
		t.Errorf("the published grant is not visible: %+v", s)
	}
}

// The added scope: review-round open counts a published plan write as written, answers the packet
// with the warning appended, and leaves the round in flight. On dev the error is returned and the
// packet is never rendered.
func TestPublishedCallersReviewRoundOpenReportsThePublishedPlan(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd := reviewRoundRunSeed(t)
	res, err := RunReviewRoundCli(*ParseReviewRoundCliArgs([]string{"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}, cwd).Args,
		&ReviewRoundRunOptions{WriteGoalplan: cliPublishedGoalplanWrite()})
	if err != nil {
		t.Fatalf("open returned an error: %v", err)
	}
	if res.Code != 0 {
		t.Fatalf("code = %d, output %q", res.Code, res.Output)
	}
	launch := reviewRoundRunRound(t, cwd).Lane.LaunchID
	if !strings.HasPrefix(res.Output, launch+"\n\nRound r1 is in flight over 1 file(s).") {
		t.Errorf("the packet changed: %q", res.Output)
	}
	if !strings.HasSuffix(res.Output, "\n"+cliPublishedWantGoalplanWarning(reviewRoundRunSlug)) {
		t.Errorf("output = %q, want the appended warning %q", res.Output, cliPublishedWantGoalplanWarning(reviewRoundRunSlug))
	}
	if got := reviewRoundRunRound(t, cwd).Status; got != goalplan.ReviewInFlight {
		t.Errorf("the published round is not in flight: %s", got)
	}
}

// The abort side: a published plan write answers the existing success with the warning appended
// and the round is closed.
func TestPublishedCallersReviewRoundAbortReportsThePublishedPlan(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd := reviewRoundRunSeed(t)
	if res := reviewRoundRunOpenDoc(t, cwd); res.Code != 0 {
		t.Fatalf("seed open: %+v", res)
	}
	res, err := RunReviewRoundCli(*ParseReviewRoundCliArgs([]string{"abort", "--session", "rb"}, cwd).Args,
		&ReviewRoundRunOptions{WriteGoalplan: cliPublishedGoalplanWrite()})
	if err != nil {
		t.Fatalf("abort returned an error: %v", err)
	}
	if res.Code != 0 || !strings.HasPrefix(res.Output, "review-round abort: r1 closed as inconclusive") {
		t.Fatalf("got %+v", res)
	}
	if !strings.HasSuffix(res.Output, "\n"+cliPublishedWantGoalplanWarning(reviewRoundRunSlug)) {
		t.Errorf("output = %q, want the appended warning %q", res.Output, cliPublishedWantGoalplanWarning(reviewRoundRunSlug))
	}
	if got := reviewRoundRunRound(t, cwd).Status; got != goalplan.ReviewInconclusive {
		t.Errorf("the published abort is not visible: %s", got)
	}
}

// A failure before the rename published nothing, so both verbs still return the error and leave
// the plan alone.
func TestPublishedCallersReviewRoundKeepsPreRenameFailure(t *testing.T) {
	for _, verb := range []string{"open", "abort"} {
		t.Run(verb, func(t *testing.T) {
			cliPublishedIsolatedHome(t)
			cwd := reviewRoundRunSeed(t)
			argv := []string{"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}
			if verb == "abort" {
				if res := reviewRoundRunOpenDoc(t, cwd); res.Code != 0 {
					t.Fatalf("seed open: %+v", res)
				}
				argv = []string{"abort", "--session", "rb"}
			}
			before := reviewRoundRunBytes(t, cwd)
			res, err := RunReviewRoundCli(*ParseReviewRoundCliArgs(argv, cwd).Args,
				&ReviewRoundRunOptions{WriteGoalplan: cliPublishedPlainGoalplanWrite()})
			if err == nil || !errors.Is(err, syscall.EIO) || state.Published(err) {
				t.Fatalf("want a plain pre-rename error: %+v %v", res, err)
			}
			if res != (ReviewRoundCliResult{}) {
				t.Errorf("a pre-rename failure returned a result: %+v", res)
			}
			if after := reviewRoundRunBytes(t, cwd); after != before {
				t.Error("a pre-rename failure changed the plan")
			}
		})
	}
}

// The seams are arguments, not package state: the production entry points keep their signatures
// and use the real writers, so a clean write carries no warning.
func TestPublishedCallersDefaultSeamsAreTheRealWriters(t *testing.T) {
	cliPublishedIsolatedHome(t)
	cwd, _, _ := cliSeed(t)
	if out, code := RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd}); code != 0 || strings.Contains(out, "published but") {
		t.Fatalf("RunMemoryCLI: %d %q", code, out)
	}
	cwd2 := reviewRoundRunSeed(t)
	if res := reviewRoundRunDo(t, cwd2, "open", "--session", "rb", "--plan-path", reviewRoundRunDoc); res.Code != 0 || strings.Contains(res.Output, "published but") {
		t.Fatalf("open with a nil seam: %+v", res)
	}
}
