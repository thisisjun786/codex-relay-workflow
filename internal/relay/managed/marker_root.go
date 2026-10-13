package managed

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// requireOwnerMarkerRoot refuses, before anything is reserved or created, a new request whose marker root
// is not the one the relay daemon judges Stops under (hook.OwnerMarkerRoot, the selection control.sock
// applies): a child's claim under another root is rejected by the daemon and its completion record is
// silently lost (claim_uncorrelated, CRW-1309). The root is compared as the daemon compares it, on the
// request's resolved root (absolute, symlinks followed) and on the owner's root resolved the same way, so
// a spelling of the same directory starts. The refusal words the owner's root and the one named as the
// hook side words them.
func (m *Start) requireOwnerMarkerRoot(id Identity) error {
	if m.OwnerMarkerRoot == "" {
		return nil
	}
	if hook.NamesOwnerMarkerRoot(id.MarkerRoot, m.OwnerMarkerRoot) {
		return nil
	}
	if owner, err := canonicalMarkerPath(m.OwnerMarkerRoot); err == nil && owner == id.MarkerRoot {
		return nil
	}
	return refusal("marker_root_mismatch", hook.MarkerRootRefusal(m.OwnerMarkerRoot, m.MarkerRoot)+
		"; managed-start --marker-root must name that root, or the child's claims are never correlated (resolved: "+id.MarkerRoot+")")
}
