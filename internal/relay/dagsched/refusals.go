package dagsched

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// refuse is a refusal under an existing reason (D-02: this build registers no new reason). The relay's exit code is 2 and
// the reason is machine readable; the closed scheduler reason, when there is one, travels in the detail.
func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &store.RefusedError{Reason: string(reason), Detail: fmt.Sprintf(format, args...)}
}

// orderedString is a string field of an ordered object, or "" when the key is absent or not a string.
func orderedString(o contract.OrderedObject, key string) string {
	for _, f := range o {
		if f.Key == key {
			s, _ := f.Value.(string)
			return s
		}
	}
	return ""
}
