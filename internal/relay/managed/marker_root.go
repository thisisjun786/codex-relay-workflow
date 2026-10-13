package managed

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// requireOwnerMarkerRoot refuses, before anything is reserved or created, a new request whose marker root
// is not the one the relay daemon judges Stops under (hook.OwnerMarkerRoot, the selection control.sock
// applies): a child's claim under another root is rejected by the daemon and its completion record is
// silently lost (claim_uncorrelated, CRW-1309). The owner's root is the one the daemon answered over
// control.sock, evaluated in the daemon's environment, which the caller's need not share; only when the daemon
// could not be asked is it this process's own resolution, and the refusal then says that (OwnerMarkerRootUnasked).
// The root is compared as the daemon compares it, on the request's resolved root (absolute, symlinks followed)
// and on the owner's root resolved the same way, so a spelling of the same directory starts. The refusal words
// the owner's root and the one named as the hook side words them.
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
	source := "the relay daemon answered that root"
	if m.OwnerMarkerRootUnasked != "" {
		source = "the relay daemon could not be asked (" + m.OwnerMarkerRootUnasked + "), so that is the root this process's environment resolves"
	}
	return refusal("marker_root_mismatch", hook.MarkerRootRefusal(m.OwnerMarkerRoot, m.MarkerRoot)+
		"; managed-start --marker-root must name that root, or the child's claims are never correlated ("+source+"; resolved: "+id.MarkerRoot+")")
}
