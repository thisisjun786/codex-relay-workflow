package affordance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// object preserves JSON.parse's surrogate strings and unused out-of-range numbers.
func object(raw string) map[string]any {
	v, err := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil
	}
	o, _ := v.(map[string]any)
	return o
}

func rootStamp(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// sessionBytes counts JavaScript UTF-16 units and supplies Node's UTF-8 encoding:
// each lone surrogate becomes one replacement character when hashing, not three.
func sessionBytes(s string) (string, int) {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
		i += size
		n++
		if r > 0xffff {
			n++
		}
		if pyjson.IsSurrogate(r) {
			r = 0xfffd
		}
		b.WriteRune(r)
	}
	return b.String(), n
}

// RecoveryPath validates a root event before creating anything. The workspace
// itself is canonicalized; .crw and its recovery directory may not be symlinks.
// These are the oracle's pathname checks, not protection against concurrent swaps.
func RecoveryPath(raw, event string, create bool) string {
	p := object(raw)
	cwd, _ := p["cwd"].(string)
	sid, _ := p["session_id"].(string)
	encoded, units := sessionBytes(sid)
	if p["hook_event_name"] != event || !filepath.IsAbs(cwd) || text.Trim(sid) == "" || units > 256 ||
		!rootStamp(p["agent_id"]) || !rootStamp(p["agent_type"]) {
		return ""
	}
	ws, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return ""
	}
	state := filepath.Join(ws, crwdir.DirName)
	dir := filepath.Join(state, "affordance-recovery")
	for _, path := range []string{state, dir} {
		st, err := os.Lstat(path)
		if err != nil {
			if !create || !os.IsNotExist(err) {
				return ""
			}
			if path == state {
				_, err = crwdir.EnsureDir(ws)
			} else {
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				return ""
			}
			st, err = os.Lstat(path)
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return ""
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%x.pending", sha256.Sum256([]byte(encoded))))
}

// RunPostCompactAffordance coalesces compactions by exclusive creation. No
// PostCompact envelope is supported; failures lose only the advisory hint.
func RunPostCompactAffordance(raw string) string {
	if path := RecoveryPath(raw, "PostCompact", true); path != "" {
		if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			_, _ = f.WriteString("pending\n")
			_ = f.Close()
		}
	}
	return ""
}

// RoleReader answers what a verified registry read says the hook's session is (harness.RegistryPromptRole in production). It is
// read-only and answers pabcdhook.PromptRoleUnknown for anything it cannot verify; a nil reader is an unknown role.
type RoleReader func(cwd, sessionID string) pabcdhook.PromptRole

// coordinates is whether the registry verifies the session as a coordinating seat (a project parent or a supervisor), whose hooks
// carry no loop pointer (CRW-386, decision 6: a non-implementing seat does not perform task work). The registry decides the role of any
// session id the payload names, as it does for Stop and UserPromptSubmit: validSessionID is the rule for printing an id into a
// pointer, not for whether the id is a session. A missing id, no reader and every role but parent and supervisor keep the
// oracle's pointers, and so does a seat that took an explicit request to implement a task in this session
// (pabcdhook.ImplementsHere): it is that task's implementer, and the state is read only for a seat the registry verified.
func (r RoleReader) coordinates(cwd, sessionID string) bool {
	return r != nil && sessionID != "" && r(cwd, sessionID).Coordinates() && !pabcdhook.ImplementsHere(cwd, sessionID)
}

// RunUserPromptAffordance is RunUserPromptAffordanceWithRole with no role reader: every session gets the whole restore.
func RunUserPromptAffordance(raw string, env host.LookupEnv) string {
	return RunUserPromptAffordanceWithRole(raw, env, nil)
}

// RunUserPromptAffordanceWithRole consumes only a regular marker. Concurrent prompts
// have one unlink winner, and a child cannot consume its parent's queued hint. The restore after a compaction omits the loop
// pointer for a session the registry verifies as a coordinating seat (CRW-386): without that, SessionStart would withhold the
// pointer from a parent and the first prompt after a compaction would bring it back. The role is read only once the marker is
// consumed, so a prompt with no marker reads nothing.
func RunUserPromptAffordanceWithRole(raw string, env host.LookupEnv, role RoleReader) string {
	path := RecoveryPath(raw, "UserPromptSubmit", false)
	if path == "" {
		return ""
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || os.Remove(path) != nil {
		return ""
	}
	sections := []harness.ContextSection{{Text: RenderBackgroundTerminalAffordance()}}
	p := object(raw)
	cwd, _ := p["cwd"].(string)
	sid, _ := p["session_id"].(string)
	if !role.coordinates(cwd, sid) {
		sections = append(sections, harness.ContextSection{Text: RenderLoopAffordance(env), Required: true})
	}
	sections = append(sections, harness.ContextSection{Text: RenderStackedPrAffordance()}, harness.ContextSection{Text: RenderQuestionAffordance()})
	return harness.ContextOutputSections("UserPromptSubmit", sections)
}

// RunMapAffordanceSessionStart keeps unconditional pointers on malformed stdin or a failed walk. Only this handler trims before
// JSON.parse. It renders the answer and records nothing: the hook that writes the answer records it (runHook), so guidance
// counts as given only once it reached the host (see mapAffordanceSessionStart). No role reader: every session gets every pointer.
func RunMapAffordanceSessionStart(raw, fallbackCwd string, env host.LookupEnv) string {
	answer, _ := mapAffordanceSessionStart(raw, fallbackCwd, env, nil)
	return answer
}

// RunMapAffordanceSessionStartWithRole is RunMapAffordanceSessionStart with the registry role reader (CRW-386): a session the
// registry verifies as a project parent or a supervisor gets no loop pointer, which tells an implementer how to arm and run a
// loop. Malformed stdin has no session, so it keeps every pointer.
func RunMapAffordanceSessionStartWithRole(raw, fallbackCwd string, env host.LookupEnv, role RoleReader) string {
	answer, _ := mapAffordanceSessionStart(raw, fallbackCwd, env, role)
	return answer
}

// mapAffordanceLeg names this leg's record of what a session was given.
const mapAffordanceLeg = "map-affordance"

// mapAffordanceSessionStart returns the answer and the function that notes what it gave, told whether the answer was written whole. A resume (CRW-1146)
// re-issues only the session binding and the PATH banner when the session was given exactly these pointers before; it holds them from
// its start or its last compact. Resume alone proves nothing (the switch can be off at the start and on at the resume, and the pointers
// name the command and the workspace size), so a resume of a session that was not given them, or given others, gets them whole. A
// missing or unknown source always gets the whole list, except the compact start that follows, in the same turn, a resume that gave the
// whole list (CRW-1180): that one adds nothing, once.
func mapAffordanceSessionStart(raw, fallbackCwd string, env host.LookupEnv, role RoleReader) (string, func(written bool)) {
	cwd, sid, source, transcript := fallbackCwd, "", "", ""
	if p := object(text.Trim(raw)); p != nil {
		if s, ok := p["cwd"].(string); ok && s != "" {
			cwd = s
		}
		sid, _ = p["session_id"].(string)
		source, _ = p["source"].(string)
		transcript, _ = p["transcript_path"].(string) // the evidence of the turn a resume and a compact run in (CRW-1180)
	}
	resumed := source == "resume"
	lines := []harness.ContextSection{}
	if validSessionID(sid) {
		lines = append(lines, harness.ContextSection{Text: RenderSessionBinding(sid, env), Required: true})
	}
	// The pointers as said, and as compared with a session's record: with the command spelled as one fixed word (the record keeps
	// the command in a field of its own, a path that differs per host) and the map pointer's file count, a size hint that moves
	// with every new file, left out.
	words := func(k string) (string, bool) {
		if k == host.BinEnv {
			return "crw", true
		}
		return env(k)
	}
	// CRW-386: a verified coordinating seat (project parent, supervisor) is given no loop pointer, in the list and in its record.
	coordinating := role.coordinates(cwd, sid)
	fixed := func(env host.LookupEnv) []string {
		list := []string{RenderSkillSearchAffordance(env), RenderKwriteAffordance()}
		if !coordinating {
			list = append(list, RenderLoopAffordance(env))
		}
		return append(list, RenderStackedPrAffordance(), RenderBackgroundTerminalAffordance(), RenderQuestionAffordance())
	}
	pointers, identity := fixed(env), fixed(words)
	if count := CountSourceFiles(cwd); count >= MapAffordanceMinFiles {
		pointers = append([]string{RenderMapAffordance(count, env)}, pointers...)
		identity = append([]string{RenderMapAffordance(MapAffordanceMinFiles, words)}, identity...)
	}
	banner := []harness.ContextSection{}
	command := invocation(env)
	if command != "crw" {
		banner = append(banner, harness.ContextSection{Text: "[crw] `crw` is not on PATH here; wherever docs say `crw`, run: " + command})
	}
	// CRW-1324: a policy file that no longer matches the bridge registration costs the session its bridge; like the PATH banner it is said
	// at every start and resume, because it can change between them.
	banner = append(banner, policyDriftSections(env)...)
	given := strings.Join(identity, "\n\n")
	// What a resume that was given the pointers before says is the binding and the PATH banner: the part of the answer that does not
	// depend on the pointers.
	headText := func() string {
		texts := []string{}
		for _, section := range append(append([]harness.ContextSection{}, lines...), banner...) {
			texts = append(texts, section.Text)
		}
		return strings.Join(texts, "\n\n")
	}
	// An answer written whole is recorded (which ends a pair an earlier resume left open, and a resume leaves one of its own); one that
	// was not written leaves no record, and no pair either, since the context does not hold what it would have said.
	record := func(written bool) {
		switch {
		case sid == "":
		case !written:
			guidancerecord.ClearResume(env, sid, mapAffordanceLeg)
		case resumed:
			guidancerecord.RecordResume(env, sid, mapAffordanceLeg, given, command, transcript)
		default:
			guidancerecord.Record(env, sid, mapAffordanceLeg, given, command)
		}
	}
	// CRW-1180: the compact start right after a resume of its turn adds only what that resume did not give: Codex keeps the resume's
	// output after the compaction record, so saying it again stacks it twice.
	pair, part := guidancerecord.PairNone, ""
	if source == "compact" && sid != "" {
		pair, part = guidancerecord.TakePair(env, sid, mapAffordanceLeg, given, command, transcript)
	}
	if pair == guidancerecord.PairWhole {
		return "", func(bool) {}
	}
	if pair == guidancerecord.PairPart && guidancerecord.PartDigest(headText()) == part {
		// The resume gave this binding and banner; the context still holds them, and the compaction emptied only the pointers.
		lines, banner = nil, nil
	}
	if resumed && sid != "" && guidancerecord.Delivered(env, sid, mapAffordanceLeg, given, command) {
		// This resume gave the binding only (and the banner): a compact of its turn must say the pointers, and only them.
		head := headText()
		lines = append(lines, banner...)
		return harness.ContextOutputSections("SessionStart", lines), func(written bool) {
			if !written {
				head = ""
			}
			guidancerecord.RecordResumePart(env, sid, mapAffordanceLeg, head, transcript)
		}
	}
	for i, pointer := range pointers {
		lines = append(lines, harness.ContextSection{Text: pointer, Required: identity[i] == RenderLoopAffordance(words)})
	}
	lines = append(lines, banner...)
	return harness.ContextOutputSections("SessionStart", lines), record
}

// RunHook bounds physical stdin before decoding, observation or handlers. Input
// failures release advisory hooks silently; cancellation prevents late effects.
func RunHook(ctx context.Context, event string, in io.Reader, out io.Writer, env host.LookupEnv, cwd string) int {
	done := make(chan int, 1)
	go func() { done <- runHook(ctx, event, in, out, env, cwd) }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		return harness.Interrupted
	}
}

func runHook(ctx context.Context, event string, in io.Reader, out io.Writer, env host.LookupEnv, cwd string) int {
	input := harness.ReadInput(in)
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	if input.Failed() {
		return 0
	}
	raw := input.Raw
	harness.RecordInvocation(raw, "cxc-ops", event, env)
	var answer string
	switch event {
	case "session-start":
		var record func(written bool)
		answer, record = mapAffordanceSessionStart(raw, cwd, env, harness.RegistryPromptRole)
		n, err := io.WriteString(out, answer)
		// Only an answer written whole counts as given: a failed or short write leaves no record, so the session's next resume gets
		// the whole list. A cancelled hook has no effects at all.
		if ctx.Err() == nil {
			record(err == nil && n == len(answer))
		}
		answer = ""
	case "post-compact":
		answer = RunPostCompactAffordance(raw)
	case "user-prompt-submit":
		// CRW-1180: a prompt of a later turn ends the resume pair, so a compaction of that turn says its guidance again.
		if p := object(raw); p != nil {
			session, _ := p["session_id"].(string)
			turn, _ := p["turn_id"].(string)
			guidancerecord.NoteUserPrompt(env, session, turn)
		}
		answer = RunUserPromptAffordanceWithRole(raw, env, harness.RegistryPromptRole)
	}
	if answer != "" {
		_, _ = io.WriteString(out, answer)
	}
	return 0
}
