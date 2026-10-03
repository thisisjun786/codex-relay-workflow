package gate

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// CheckGateResult is the verdict of ValidateCheckReceipt: Reason says why not when OK is false.
type CheckGateResult struct {
	OK     bool
	Reason string
}

// ValidateCheckReceipt verifies the receipt a C>D attestation names (receiptPath is empty when it names none), for bound sessions
// only. Beyond the shared parser it asks a command, exit code 0, a createdAt, this session and this check cycle, and a source tree
// that has not moved since, compared with the exclusions the receipt was captured with (else a declared path that kept changing
// would make the edge unpassable).
func ValidateCheckReceipt(st state.State, sessionID, receiptPath, cwd string) CheckGateResult {
	refuse := func(why string) CheckGateResult {
		return CheckGateResult{Reason: why + " (CHECK-BINDING-01). Produce one with: crw pabcd receipt test --session <id> -- <command>, then pass its path as testReceiptPath."}
	}
	if receiptPath == "" {
		return refuse(`C -> D on a goalplan-bound session requires "testReceiptPath"`)
	}
	if st.CheckEpoch == nil || *st.CheckEpoch == "" {
		return CheckGateResult{Reason: "this check cycle predates CHECK-BINDING-01, so no receipt can bind to it. Step back with `crw pabcd orchestrate B` and re-enter `crw pabcd orchestrate C` to mint a binding."}
	}
	parsed, err := ParseSourceBoundReceipt(receiptPath, cwd, ReceiptTest)
	if err != nil {
		return refuse(err.Error())
	}
	switch {
	case parsed.Command == nil || text.Trim(*parsed.Command) == "":
		return refuse("the receipt names no command, so it records nothing about what ran")
	case parsed.ExitCode == nil:
		return refuse("the receipt reports exitCode none; only a passing run closes a check")
	case *parsed.ExitCode != 0:
		return refuse(fmt.Sprintf("the receipt reports exitCode %s; only a passing run closes a check", jsNumberText(*parsed.ExitCode)))
	case !parsed.CreatedAtProvided:
		return refuse("the receipt carries no usable createdAt")
	case parsed.OwnerSessionID == nil || *parsed.OwnerSessionID != sessionID:
		return refuse("the receipt was produced by a different session")
	case parsed.CheckEpoch == nil || *parsed.CheckEpoch != *st.CheckEpoch:
		return refuse("the receipt belongs to an earlier check cycle — re-run the command for this one")
	}
	exclude := true
	now, err := session.Capture(cwd, sessionID, session.CaptureOptions{ExcludeStateArtifacts: &exclude, GeneratedPaths: parsed.GeneratedPaths})
	if err != nil {
		return refuse("SOURCE-ROOT: " + err.Error())
	}
	switch cmp := source.Compare(parsed.SourceIdentity.Identity(), now); cmp.Kind {
	case source.ComparisonDifferent:
		return refuse("the source changed after the check ran (" + cmp.Detail + ")")
	case source.ComparisonUnavailable:
		return refuse("git could not resolve the source identity (" + cmp.Reason + ")")
	}
	return CheckGateResult{OK: true}
}
