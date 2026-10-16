package cli_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// TestRelayCommandWordsMatchTheRegistry keeps layername's relay command names equal to the relay
// commands this build registers whose name begins with supervisor- or parent- (CRW-1546): a command
// added, renamed or dropped here without the layername set would make Find count, or stop counting,
// a command name.
func TestRelayCommandWordsMatchTheRegistry(t *testing.T) {
	var registered []string
	for _, name := range dispatch.Names() {
		if strings.HasPrefix(name, "supervisor-") || strings.HasPrefix(name, "parent-") {
			registered = append(registered, name)
		}
	}
	sort.Strings(registered)
	if len(registered) == 0 {
		t.Fatal("no relay command named supervisor-* or parent-* is registered")
	}
	if got := layername.CommandNames("relay"); !reflect.DeepEqual(got, registered) {
		t.Errorf("layername.CommandNames(\"relay\") = %q, want the registered %q", got, registered)
	}
}
