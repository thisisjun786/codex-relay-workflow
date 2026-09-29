package faults

import (
	"bytes"
	"testing"
)

// FLT-34: compare complete page bytes, with input clock/token injection.
func TestDNotificationsPageWholeBytesAgainstPython(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	check := func(args ...string) map[string]any { t.Helper(); return f1ReplayCLI(t, ctx, gd, pd, args) }
	seed := check("fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"observation_unmeasured","severity":"notice","signature":{"relationship":"rel-1","turn":"turn-1"},"occurrenceKey":"u1","scope":{"projectKey":"CRW"}}`)
	for _, reason := range []string{"owner_hold", "project_hold", "classification"} {
		check("fault-notification-raise", "--fault", seed["faultId"].(string), "--reason", reason)
	}
	check("fault-notifications", "--limit", "2")
	check("fault-notifications", "--limit", "2", "--after", "2")
	var stderr bytes.Buffer
	code, handled := executeAsCLI(ctx, []string{"--state", gd, "--json", "fault-notifications", "--after", "not-a-cursor"}, &bytes.Buffer{}, &stderr)
	if !handled || code != 2 || stderr.Len() == 0 {
		t.Fatalf("invalid cursor: %d %q", code, stderr.String())
	}
}
