package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The host settings of a managed child start (SP-CHILD-1, CRW-1285), kept for schedrelay.go until bundle S. No start takes them any
// more: `crw flow start` has no --child-host and a run's lane reads no "child_host" argument, so a run holds no host settings and a
// node sized for a child is refused with the reason (ErrChildHostUnset, an input error), never answered as a child that is not running.

// ChildHostSchema is the schema of the document.
const ChildHostSchema = "crw-flow-child-host/1"

// ChildHost is the document: the marker root of the managed start, and the parent's and the child's endpoint (dag-release-request/1
// parent and child: the host, the settings each task runs with, the child's title).
type ChildHost struct {
	Schema     string            `json:"schema"`
	MarkerRoot string            `json:"marker_root"`
	Parent     dagsched.Endpoint `json:"parent"`
	Child      dagsched.Endpoint `json:"child"`
}

// ErrChildHostUnset is the refusal of a node sized for a child in a run that holds no host settings.
var ErrChildHostUnset = errors.New("refused:child_host_unset: the run holds no host settings for a managed child start (SP-CHILD-1): no start gives a run a " +
	ChildHostSchema + " document any more")

// Problem says why the document cannot start a managed child ("" when it can): the schema, an absolute marker root, the hosts, the
// settings and the child's title, one host for both (a managed start supports one local host). The settings themselves are judged by the
// managed start's own reading of the request (managed.ParseRequest), when a release assembles it.
func (h ChildHost) Problem() string {
	var missing []string
	if h.Schema != ChildHostSchema {
		return fmt.Sprintf("the document's schema is %q, not %s", h.Schema, ChildHostSchema)
	}
	if !filepath.IsAbs(h.MarkerRoot) {
		missing = append(missing, "marker_root (an absolute path)")
	}
	for _, f := range []struct {
		name  string
		empty bool
	}{{"parent.host_id", strings.TrimSpace(h.Parent.HostID) == ""}, {"parent.settings", len(h.Parent.Settings) == 0},
		{"child.host_id", strings.TrimSpace(h.Child.HostID) == ""}, {"child.title", strings.TrimSpace(h.Child.Title) == ""}, {"child.settings", len(h.Child.Settings) == 0}} {
		if f.empty {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return "the child host settings name no " + strings.Join(missing, ", ")
	}
	if h.Parent.HostID != h.Child.HostID {
		return fmt.Sprintf("the 자식's host %s is not the `child` session's %s: a managed start supports one local host", h.Parent.HostID, h.Child.HostID)
	}
	return ""
}
