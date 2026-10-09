package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CliResult is the terminal answer of orchestrate-cli.ts:421, without a trailing newline.
type CliResult struct {
	Code   int    `json:"code"`
	Output string `json:"output"`
}

// ReadEnv keeps the caller's native identity separate from the process homedir.
// Nil Native means no variables, as the oracle library's default {} does;
// nil Process uses os.LookupEnv for os.homedir().
type ReadEnv struct{ Native, Process host.LookupEnv }

// OrchestrateReadResult delegates a validated mutation when Result is nil. The
// transition owner then reads SessionID's state; this library never writes it.
type OrchestrateReadResult struct {
	Result    *CliResult
	SessionID string
}

// TerminalSessionKey is the only reserved bootstrap key (RESERVED_SESSION_KEYS).
const TerminalSessionKey = "cli"

func IsReservedSessionKey(id string) bool { return id == TerminalSessionKey }

func nonemptySession(id *string) bool { return id != nil && *id != "" }

// ResolveSession ports orchestrate-cli.ts:301-316. Explicit nonempty ids win;
// discovery picks any stat-able .json entry, newest mtimeMs then JS lexical id.
// Listing errors propagate, as the oracle's readdirSync throws on that path.
func ResolveSession(cwd string, explicit *string) (*string, error) {
	if nonemptySession(explicit) {
		return explicit, nil
	}
	dir := filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir)
	if _, err := os.Stat(dir); err != nil {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var best *string
	var bestMtime float64
	for _, entry := range entries {
		name := source.DecodeUTF8([]byte(entry.Name()))
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		stamp := info.ModTime()
		mtime := float64(stamp.Unix())*1000 + float64(stamp.Nanosecond())/1e6
		if best == nil || mtime > bestMtime || mtime == bestMtime && slices.Compare(utf16.Encode([]rune(id)), utf16.Encode([]rune(*best))) < 0 {
			best, bestMtime = &id, mtime
		}
	}
	return best, nil
}

// SessionFileExists ports :328-330. This intentionally checks the raw id, while
// ReadState sanitizes it: the oracle's mismatched paths remain observable.
func SessionFileExists(cwd, sessionID string) bool {
	_, err := os.Stat(filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir, sessionID+".json"))
	return err == nil
}

func RenderPhaseContext(s state.State, sessionID string) string {
	return fmt.Sprintf("current=%s session=%s", s.Phase, sessionID)
}

// RenderStatus ports :389-412, including the optional split-tree warning.
func RenderStatus(s state.State, asJSON bool, elsewhere []string, selection string) (string, error) {
	if asJSON {
		return statusJSON(struct {
			Phase       state.Phase `json:"phase"`
			Flags       state.Flags `json:"flags"`
			SessionID   string      `json:"sessionId"`
			Selection   string      `json:"selection"`
			AlsoFoundAt []string    `json:"alsoFoundAt,omitempty"`
		}{s.Phase, s.Flags, s.SessionID, selection, elsewhere})
	}
	line := fmt.Sprintf("session=%s phase=%s interview=%t auditPassed=%t checkPassed=%t", s.SessionID, s.Phase, s.Flags.Interview, s.Flags.AuditPassed, s.Flags.CheckPassed)
	if selection == "latest-file" {
		line += " selection=latest-file (unverified terminal fallback)"
	}
	if len(elsewhere) == 0 {
		return line, nil
	}
	lines := []string{line, fmt.Sprintf("WARNING: this session id also has state in %d other tree(s); the phase above describes THIS cwd only.", len(elsewhere))}
	for _, path := range elsewhere {
		lines = append(lines, "  also at: "+path)
	}
	return strings.Join(append(lines, "  Pass --cwd <path> to address a specific tree."), "\n"), nil
}

// RenderOrchestrateParseError ports :414-419, preserving explicit phase context.
func RenderOrchestrateParseError(e CliParseError) string {
	if nonemptySession(e.Session) && SessionFileExists(e.Cwd, *e.Session) {
		return "orchestrate: " + RenderPhaseContext(state.ReadState(e.Cwd, *e.Session), *e.Session) + "; " + e.Error
	}
	return "orchestrate: " + e.Error
}

// SiblingRoots ports :1139-1160; home scan failure still permits the cwd parent,
// but a homedir lookup failure returns immediately. Symlink directories are skipped.
func SiblingRoots(cwd string, process host.LookupEnv) []string {
	if process == nil {
		process = os.LookupEnv
	}
	roots := []string{}
	home, err := host.Home(process)
	if err != nil {
		return roots
	}
	if entries, err := os.ReadDir(home); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == "node_modules" || entry.Name() == "AppData" {
				continue
			}
			roots = append(roots, filepath.Join(home, source.DecodeUTF8([]byte(entry.Name()))))
		}
	}
	abs, err := filepath.Abs(cwd)
	if err == nil {
		if parent := filepath.Dir(abs); parent != abs {
			roots = append(roots, parent)
		}
	}
	return roots
}

// RunOrchestrateRead ports the read-only prefix (:466-548) on a parser result:
// help/errors/status terminate; passed mutation guards delegate to the transition
// library. Native identity selects the session of an implicit status and bounds an explicit mutation
// to the native session (nativeSessionMismatch); hooks never pass it.
func RunOrchestrateRead(parsed OrchestrateCliParsed, env ReadEnv) (OrchestrateReadResult, error) {
	if parsed.Help != nil {
		return readAnswer(0, RenderOrchestrateHelp("")), nil
	}
	if parsed.Error != nil {
		return readAnswer(1, RenderOrchestrateParseError(*parsed.Error)), nil
	}
	a := parsed.Args
	// CRW-871: a mutating verb's explicit id is judged before anything else, the attestation-error
	// branch included: that branch renders the sanitised session's phase with RenderPhaseContext, which
	// reads a DIFFERENT session's file for a raw id. status and the reset/attest-exempt paths below keep
	// the oracle's order; only the mutating verbs move their canonical check ahead.
	if a.Verb != fsm.VerbStatus && nonemptySession(a.Session) && !state.IsCanonicalSessionID(*a.Session) {
		return readAnswer(1, sessionAliasRefusalOutput(a.Verb)), nil
	}
	if env.Native == nil {
		env.Native = func(string) (string, bool) { return "", false }
	}
	// CRW-1108 (B2-01): inside a native session a mutation may change only that session. Judged right
	// after the id's shape and before the attestation-error branch, which would read the other
	// session's phase, so a refused id is neither read nor written.
	if a.Verb != fsm.VerbStatus && nonemptySession(a.Session) {
		if refusal := nativeSessionMismatch(*a, env.Native); refusal != "" {
			return readAnswer(1, refusal), nil
		}
	}
	if a.AttestError != "" && a.Verb != fsm.VerbStatus && a.Verb != fsm.VerbReset {
		context, hint := "", ""
		var from *state.Phase
		if nonemptySession(a.Session) && SessionFileExists(a.Cwd, *a.Session) {
			s := state.ReadState(a.Cwd, *a.Session)
			from = &s.Phase
			context = RenderPhaseContext(s, *a.Session) + "; "
		}
		if strings.Contains(a.AttestError, "missing valid from/to") {
			hint = RenderAttestShapeHint(a.Verb, from)
		}
		return readAnswer(1, "orchestrate "+VerbText(a.Verb)+": "+context+a.AttestError+"."+hint), nil
	}
	_, hasNative := env.Native("CODEX_THREAD_ID")
	var sessionID *string
	if a.Verb == fsm.VerbStatus && !nonemptySession(a.Session) && hasNative {
		native, err := host.ResolveNativeSession(a.Cwd, env.Native)
		if err != nil {
			if !a.JSON {
				return readAnswer(1, "orchestrate status: "+err.Error()), nil
			}
			output, encodeErr := statusJSON(struct {
				Error string       `json:"error"`
				Phase *state.Phase `json:"phase"`
			}{Error: err.Error()})
			return readAnswer(1, output), encodeErr
		}
		sessionID = &native.SessionID
	} else {
		var err error
		sessionID, err = ResolveSession(a.Cwd, a.Session)
		if err != nil {
			return OrchestrateReadResult{}, err
		}
	}
	if a.Verb == fsm.VerbStatus {
		return readStatus(*a, sessionID, hasNative, env.Process)
	}
	if !nonemptySession(a.Session) {
		return readAnswer(1, "orchestrate "+VerbText(a.Verb)+": mutating verbs require an explicit --session <id> (your codex session id from the SessionStart context line, or the terminal key 'cli'). The implicit most-recent-session fallback is disabled for writes: a concurrent or forked session must never mutate another session's FSM."), nil
	}
	if !nonemptySession(sessionID) {
		return readAnswer(1, "orchestrate "+VerbText(a.Verb)+": no active session — pass --session <id> (the codex session id, or an explicit terminal session like 'cli')"), nil
	}
	if !SessionFileExists(a.Cwd, *sessionID) && !IsReservedSessionKey(*sessionID) {
		return readAnswer(1, fmt.Sprintf("orchestrate %s: unknown session '%s' — no .crw/sessions/%s.json exists. Run crw relay session current and crw relay session bind in the native session cwd; use 'cli' only for a standalone terminal.", VerbText(a.Verb), *sessionID, *sessionID)), nil
	}
	return OrchestrateReadResult{SessionID: *sessionID}, nil
}

func readAnswer(code int, output string) OrchestrateReadResult {
	return OrchestrateReadResult{Result: &CliResult{Code: code, Output: output}}
}

// nativeSessionMismatch is the refusal of a mutation whose explicit --session is not the native
// session the command runs in, or "" when the mutation may proceed (CRW-1108). The oracle checks
// native identity for implicit status only, so a native session could change a parent's, a sibling's
// or an earlier session's FSM with exit 0 (orchestrate-cli.ts:489; port: fixed).
//
// The subject is CODEX_THREAD_ID, set and nonempty; without it (a standalone terminal) the explicit
// id, the reserved terminal key included, keeps working. The absorbed resolver confirms the subject
// against the native thread database where it can, but a lookup that fails (no database, another
// working directory, a worktree) is not widened into a refusal: the alias guard is the id comparison
// alone, so an independent terminal flow that cannot reach the database is not broken. Only the
// terminal row supplies Native; hooks never do, because a subagent's CODEX_THREAD_ID is not the root
// session id its hook payload carries.
func nativeSessionMismatch(a OrchestrateCliArgs, native host.LookupEnv) string {
	threadID, set := native("CODEX_THREAD_ID")
	if !set || threadID == "" || *a.Session == threadID {
		return ""
	}
	confirmed := ""
	if resolved, err := host.ResolveNativeSession(a.Cwd, native); err == nil && resolved.SessionID == threadID {
		confirmed = ", confirmed by the native thread database"
	}
	return fmt.Sprintf("orchestrate %s: --session '%s' is not the native Codex session this command runs in (CODEX_THREAD_ID %s%s). SESSION-IDENTITY-01: a mutation may change only your own session, never a parent, sibling or earlier session id or the terminal key 'cli'; pass your own id (crw relay session current shows it). Nothing was written.", VerbText(a.Verb), *a.Session, threadID, confirmed)
}

func readStatus(a OrchestrateCliArgs, sessionID *string, hasNative bool, process host.LookupEnv) (OrchestrateReadResult, error) {
	if !nonemptySession(sessionID) {
		return readAnswer(0, "no active session"), nil
	}
	if !SessionFileExists(a.Cwd, *sessionID) {
		const message = "session state is missing in this cwd; run crw relay session current and crw relay session bind from the native session cwd. Binding does not verify hook execution."
		if !a.JSON {
			return readAnswer(1, "session="+*sessionID+": "+message), nil
		}
		output, err := statusJSON(struct {
			SessionID   string       `json:"sessionId"`
			Phase       *state.Phase `json:"phase"`
			StateExists bool         `json:"stateExists"`
			Error       string       `json:"error"`
		}{SessionID: *sessionID, Error: message})
		return readAnswer(1, output), err
	}
	selection := "latest-file"
	if nonemptySession(a.Session) {
		selection = "explicit"
	} else if hasNative {
		selection = "native"
	}
	elsewhere := state.FindForeignSessionCopies(a.Cwd, *sessionID, SiblingRoots(a.Cwd, process))
	output, err := RenderStatus(state.ReadState(a.Cwd, *sessionID), a.JSON, elsewhere, selection)
	return readAnswer(0, output), err
}

// Same compact JSON.stringify string rules as state.stringify, kept local because
// the state encoder is private. Escaped backslashes stay paired during the pass.
func statusJSON(value any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	in := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out = append(out, "\u2028"...)
			i += 5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out = append(out, "\u2029"...)
			i += 5
		default:
			out = append(out, in[i], in[i+1])
			i++
		}
	}
	return string(out), nil
}
