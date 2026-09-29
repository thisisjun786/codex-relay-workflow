package hook

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// The records the adapter leaves behind, stated once beside the writers for the readers of a
// journal (crw-dev stop-events). The writers in adapter.go and ledger.go use the constants
// below; the vocabularies name every value a row can carry, including the few only the Python
// adapter wrote (a signalled guard, a kept stderr), so a journal written before cutover reads
// the same after it.
const (
	// RecordVersion is a row's recordVersion. Version 1 rows predate event identity.
	RecordVersion = 2
	// LedgerVersion is the ledgerVersion of a claim, a host file and an outcome.
	LedgerVersion = 1
	// LedgerDirectory holds a journal root's claims and outcomes.
	LedgerDirectory = "accepted"
	// OutcomeSuffix names an owner's outcome beside its claim: <key>.outcome.json.
	OutcomeSuffix = ".outcome.json"
	// StderrLimit is how much of a guard's stderr a row keeps.
	StderrLimit = 400
)

// HostLedgerParts is where under a Codex home the host's registrations meet for one Stop.
var HostLedgerParts = []string{"crw-completion-hook", "stop-events"}

// The two releases that always say one sentence.
const (
	DuplicateDetail    = "this Stop event already has its accepted record, so the guard was not asked again"
	UnarbitratedDetail = "the host's record of this Stop event could be neither made nor found, so no invocation can own it and the guard was not asked"
)

// Acceptances: what a row says became of its claim.
const (
	Accepted      = "accepted"
	Duplicate     = "duplicate"
	Unestablished = "unestablished"
	Unclaimable   = "unclaimable"
	ClaimFailed   = "claim_failed"
	Unarbitrated  = "unarbitrated"
)

// Adapter outcomes outside the guard's own.
const (
	GuardAnswered       = "guard_answered"
	GuardUnreachable    = "guard_unreachable"
	AdapterFaulted      = "adapter_faulted"
	DuplicateInvocation = "duplicate_invocation"
	ArbitrationFailed   = "arbitration_failed"
)

// BeforeTheGuard are the outcomes recorded before any guard is asked.
var BeforeTheGuard = []string{"stdin_unreadable", "stdin_not_json", "stdin_not_object", "config_absent", "config_unreadable", "config_unreachable", "config_malformed"}

// FromTheGuard are the outcomes that report asking one.
var FromTheGuard = []string{GuardUnreachable, "guard_timed_out", "guard_signalled", "guard_rejected_the_call", "guard_refused", "guard_host_error", "guard_usage_error", "guard_ended_unexpectedly", "guard_said_nothing", "guard_output_unreadable", "guard_verdict_incomplete", GuardAnswered}

// SettingsUntried are the outcomes whose row names a settings path nothing read.
var SettingsUntried = []string{"config_unreadable", "config_unreachable", AdapterFaulted}

// Process endings and stdout readings of a guard call.
const (
	NotStarted = "not_started"
	Exited     = "exited"
	Signalled  = "signalled"
	TimedOut   = "timed_out"

	SaidNothing             = "said_nothing"
	SaidAVerdict            = "said_a_verdict"
	SaidAnErrorRecord       = "said_an_error_record"
	SaidSomethingUnreadable = "said_something_unreadable"
)

var ProcessEndings = []string{NotStarted, Exited, Signalled, TimedOut}
var StdoutReadings = []string{SaidNothing, SaidAVerdict, SaidAnErrorRecord, SaidSomethingUnreadable}

// Decisions a guard answers with; Block is the one that holds.
const (
	Block   = "block"
	Release = "release"
)

var Decisions = []string{Block, Release}

// Journal policies the settings may name.
const (
	EveryInvocation = "every_invocation"
	FaultsOnly      = "faults_only"
	NoJournal       = "no_journal"
)

var JournalPolicies = []string{EveryInvocation, FaultsOnly, NoJournal}

// The reasons EventIdentity gives for an identity it could not establish.
const (
	IdentityFieldsIncomplete = "identity_fields_incomplete"
	TranscriptPathMissing    = "transcript_path_missing"
	TranscriptPathRelative   = "transcript_path_relative"
	TranscriptUnreachable    = "transcript_unreachable"
	SessionMismatch          = "session_mismatch"
)

var UnestablishedReasons = []string{IdentityFieldsIncomplete, TranscriptPathMissing, TranscriptPathRelative, "transcript_absent", TranscriptUnreachable, "transcript_not_regular", "scan_bound_exceeded", "scan_timed_out", "transcript_tail_incomplete", "transcript_line_unreadable", "turn_start_not_found", "no_answer_item_for_turn", "answer_item_unidentified", "answer_precedes_latest_input", "answer_text_mismatch", "answer_text_ambiguous", SessionMismatch}

// PathlessReasons stop before a transcript path is looked at; PathUntriedReasons also cover the
// two whose path the system never took.
var PathlessReasons = []string{IdentityFieldsIncomplete, TranscriptPathMissing}
var PathUntriedReasons = []string{IdentityFieldsIncomplete, TranscriptPathMissing, TranscriptPathRelative, TranscriptUnreachable}

// The fields each writer puts in its record. A row starts with RowFields; once the payload is
// read it gains PayloadFields; a guard call adds GuardCallFields, an answer AnswerFields; every
// row that did not fault ends with SettledFields, and a fault adds FaultFields instead.
var (
	RowFields           = []string{"recordVersion", "event", "at", "adapterOutcome", "processEnding", "stdoutReading", "guardState", "guardDecision", "guardMode", "assignmentId", "guardRecordedAs", "held", "eventKey", "eventIdentity", "identityScanMs", "acceptance", "acceptedAs", "guardInvoked", "configuration", "elapsedMs"}
	PayloadFields       = []string{"sessionId", "turnId", "stopHookActive"}
	GuardCallFields     = []string{"exitCode", "signal", "errno", "guardElapsedMs", "guardStderr"}
	AnswerFields        = []string{"observation", "counters"}
	SettledFields       = []string{"detail"}
	FaultFields         = []string{"fault", "journalledAs"}
	IdentityFields      = []string{"established", "reason", "answerItem", "transcriptPath", "scannedBytes", "scannedLines"}
	OutcomeFields       = []string{"ledgerVersion", "eventKey", "sessionId", "turnId", "journalPolicy", "adapterOutcome", "guardDecision", "guardState", "held", "attemptRow", "at"}
	ClaimFields         = []string{"ledgerVersion", "eventKey", "sessionId", "turnId", "stopHookActive", "answerItem", "claimedAt", "claimedBy"}
	ClaimedByFields     = []string{"pid", "attemptRow", "hostLedger"}
	HostClaimedByFields = []string{"pid", "journalRoot", "attemptRow"}
)

// RecordBytes is the one form every record is written in: sorted keys, ASCII, one line and a
// newline (completion._record_bytes). A file holding the same content in other bytes was not
// written by the adapter.
func RecordBytes(document any) []byte {
	return []byte(evidence.Dumps(document, false, true, true) + "\n")
}

// PathTheSystemTakes is whether the operating system takes this path at all: encodable, no
// embedded NUL, no name longer than NAME_MAX and the whole shorter than PATH_MAX. A lone
// surrogate decodes the way Python's surrogateescape encodes it: U+DC80..U+DCFF stand for one
// byte each and any other one cannot be encoded.
func PathTheSystemTakes(path string) bool {
	raw, ok := fsencode(path)
	if !ok || strings.IndexByte(raw, 0) >= 0 || len(raw) >= 4096 {
		return false
	}
	for _, name := range strings.Split(raw, "/") {
		if len(name) > 255 {
			return false
		}
	}
	return true
}

// fsencode is os.fsencode of a str as a Go string holds one: each lone surrogate U+DC80..U+DCFF,
// kept as WTF-8, back to the byte it escapes. ok is false for any other lone surrogate, which
// Python cannot encode (UnicodeEncodeError). It is internal/runtime/reading.FSEncode, which this
// package cannot import: reading imports hook.
func fsencode(path string) (string, bool) {
	var encoded strings.Builder
	for i := 0; i < len(path); {
		c := path[i]
		if i+3 <= len(path) && c == 0xed && path[i+1] >= 0xa0 && path[i+1] <= 0xbf && path[i+2] >= 0x80 && path[i+2] <= 0xbf {
			r := rune(c&0x0f)<<12 | rune(path[i+1]&0x3f)<<6 | rune(path[i+2]&0x3f)
			if r < 0xdc80 || r > 0xdcff {
				return "", false
			}
			encoded.WriteByte(byte(r - 0xdc00))
			i += 3
			continue
		}
		encoded.WriteByte(c)
		i++
	}
	return encoded.String(), true
}
