package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// hookComparedKeys is hook_probe.COMPARED_KEYS: what checkHookOneTrace compares
// when a fixture declares the key.
var hookComparedKeys = []string{"decision", "state", "observation", "reason", "receiptEvidence", "receiptDetail"}

// hookOracleRequiredKeys is hook_probe.ORACLE_REQUIRED_KEYS, declared apart from
// hookComparedKeys so a key deleted from one side cannot vanish from the other.
var hookOracleRequiredKeys = []string{"decision", "state", "observation", "reason", "receiptEvidence", "receiptDetail"}

// hookOracleSelfCheck is hook_probe._oracle_self_check: every required key is
// given a deliberately wrong expectation, and the oracle must reject it.
// Returns (failures, proven).
func hookOracleSelfCheck() ([]string, int, error) {
	return hookOracleSelfCheckWith(hookComparedKeys, hookOracleRequiredKeys)
}

func hookOracleSelfCheckWith(compared, required []string) ([]string, int, error) {
	seen := map[string]int{}
	for _, key := range compared {
		seen[key] |= 1
	}
	for _, key := range required {
		seen[key] |= 2
	}
	var difference []string
	for key, sides := range seen {
		if sides != 3 {
			difference = append(difference, key)
		}
	}
	if len(difference) > 0 {
		sort.Strings(difference)
		return []string{"the compared and the required key sets differ on " + strings.Join(difference, ", ")}, 0, nil
	}
	digest := sha256.Sum256([]byte("dispatch-0001"))
	observation, err := hook.Decode([]byte(`{"stop_input":{"cwd":"/workspace/example","session_id":"session-1111","turn_id":"turn-0001","stop_hook_active":false},` +
		`"marker":{"intent":{"declaredAt":"2026-01-01T00:00:00+00:00","dispatchRequestIdHash":"` + hex.EncodeToString(digest[:]) + `","issue":"JUN-000","workspace":"/workspace/example"},` +
		`"bound":{"at":"2026-01-01T00:05:00+00:00","sessionId":"session-1111","taskId":"task-1111"},` +
		`"claims":[{"at":"2026-01-01T00:05:00+00:00","dispatchRequestId":"dispatch-0001","factId":"claims/session-1111/claim.json","firstTurnId":"turn-0001","sessionId":"session-1111"}],` +
		`"relationship":{"at":"2026-01-01T00:05:00+00:00","relationshipId":"rel-example","executionGeneration":1}},` +
		`"disposition":{"outcome":"ready_for_review","sessionId":"session-1111","turnId":"turn-0001"},` +
		`"receipt":{"relationshipId":"rel-example","sessionId":"session-1111","turnId":"turn-0001","atCurrentHead":false,"evidence":"registration_generation_mismatch",` +
		`"detail":"the assignment registered generation 1 and the relationship now stands on generation 2"},` +
		`"now":"2026-01-01T00:05:00+00:00"}`))
	if err != nil {
		return nil, 0, err
	}
	actual, err := probeDecide(observation)
	if err != nil {
		return nil, 0, err
	}
	var failures []string
	for _, key := range required {
		if _, ok := actual[key]; !ok {
			failures = append(failures, "the self-check observation never produces "+key+", so nothing here proves it is compared")
			continue
		}
		expected := hook.Object{}
		for _, name := range required {
			if value, ok := actual[name]; ok {
				expected = append(expected, hook.Field{Key: name, Value: value})
			}
		}
		expected = objSet(expected, key, "a value this answer cannot have")
		matched, err := checkHookOneKeys("oracle self-check "+key, observation, expected, io.Discard, nil, compared)
		if err != nil {
			return nil, 0, err
		}
		if matched {
			failures = append(failures, key+" is declared compared and a wrong value was accepted")
		}
	}
	return failures, len(required), nil
}
