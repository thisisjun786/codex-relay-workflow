package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// CRW-1476 (bundle n2-layer-words): the usage refusals of the 부모 channel's commands name the
// layer (부모), never the relay role id `supervisor` written as a bare word
// (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"). The command and flag names
// stay as they are; the prose around them names the layer.
func TestSupervisorCLIUsageNamesTheLayers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", root)
	state := filepath.Join(root, "state")
	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"a recipient with a project", []string{"--state", state, "supervisor-stage", "--project", "PRJ-1", "--recipient", "01someone"}, "cannot combine --recipient and --project: --recipient names the 부모 ONE message is addressed to, "},
		{"no subject", []string{"--state", state, "supervisor-stage"}, "a staging needs a subject: --event for one event's obligation, "},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := cli.Execute(context.Background(), c.argv, &out, &errOut)
			if code != 4 {
				t.Fatalf("argv %v: exit %d, want the usage exit 4: %s%s", c.argv, code, out.String(), errOut.String())
			}
			var answer map[string]any
			if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
				t.Fatalf("argv %v: not a JSON answer: %v: %s", c.argv, err, out.String())
			}
			detail, _ := answer["detail"].(string)
			if !strings.HasPrefix(detail, c.want) {
				t.Errorf("the usage refusal %q does not start %q", detail, c.want)
			}
			if hits := layername.Find(detail); len(hits) > 0 {
				t.Errorf("the usage refusal %q names a role id where a person reads a layer: %v", detail, hits)
			}
		})
	}
}
