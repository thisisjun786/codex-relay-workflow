package role

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A promptOverride is put before the work message, so a child's first message can carry other dispatch markers before the
// marker of its own attempt. The marker of this attempt is looked for in the whole message, by the created report and by the
// issuance's look at the marked children the host already shows.
func TestDispatchReceiptAnEarlierMarkerDoesNotHideThisAttemptsMarker(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	example := "Report like this:\n[CRW-DISPATCH:example:att-1]\n"
	// The host already shows a child of this attempt's marker behind an example marker: the issuance records it.
	old := dispatchReceiptRow{id: "child-old", parent: "session-test", first: example + marker + "\nREVIEW"}
	dispatchReceiptIssueSeeing(t, ws, dispatchReceiptNative(t, env, old), marker, "call-1")
	if got := must(dispatchRead(dispatchReceiptFile(ws, "review-test"), "session-test", "review-test")).Attempts[0].PriorChildren; !reflect.DeepEqual(got, []string{"child-old"}) {
		t.Fatalf("prior children behind an example marker = %v", got)
	}
	// An example marker alone, or another attempt's marker, is still no marker of this attempt.
	other := dispatchReceiptRow{id: "child-b", parent: "session-test", first: example + "[CRW-DISPATCH:review-test:att-1]\nREVIEW"}
	seen := dispatchReceiptNative(t, env, old, other)
	dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{"child-b"}})
	if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-b"), seen, nil); err == nil || !strings.Contains(err.Error(), "carries no marker of this attempt") {
		t.Fatalf("created of a child carrying only other markers = %v", err)
	}
	mine := dispatchReceiptRow{id: "child-a", parent: "session-test", first: example + marker + "\nREVIEW"}
	seen = dispatchReceiptNative(t, env, old, mine)
	dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{"child-a"}})
	out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), seen, nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r == nil || r.Correlation != "spawn-result" {
		t.Fatalf("created of the child behind an example marker = %+v", out.Attempts[0].Receipt)
	}
}

// The issuance's look at the host runs under the record lock inside the installed hook's own time limit; it is bounded well
// below that limit so the issuance is still saved and the lock released when the look runs out.
func TestManagedSpawnLookupFitsTheInstalledHookLimit(t *testing.T) {
	hook := must(os.ReadFile(filepath.Join("..", "..", "plugins", "crw", "wiring", "hooks", "pre-tool-use-attaching-skills.json")))
	m := regexp.MustCompile(`"timeout":\s*([0-9]+)`).FindSubmatch(hook)
	if m == nil {
		t.Fatal("the installed spawn hook declares no timeout")
	}
	limit := time.Duration(must(strconv.Atoi(string(m[1])))) * time.Second
	if managedSpawnLookupBudget*3 > limit {
		t.Fatalf("the issuance lookup budget %v is more than a third of the installed hook limit %v", managedSpawnLookupBudget, limit)
	}
}
