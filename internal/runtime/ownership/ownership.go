// Package ownership is scripts/crw_runtime/ownership.py for a Go install: the OPS-2.1 signals
// and the OPS-2.2 five classes in their fixed precedence (conflict, fork, foreign, unmeasured,
// own). 'own' is the residual class and the only reusable one.
//
// A Go install is an archive, not a checkout, so the checkout signals do not exist for it:
// commit_matches was already dead in Python (runtime_install.py never filled it), and
// tree_matches and working_tree_clean describe a source tree the binary was built from, which
// is recorded in the install entry's source block rather than compared here. A fork is
// therefore bytes that differ from the recorded digest. Python installs are never classified
// by Go; they keep runtime_install.py's classification until todo 44.
package ownership

// The five classes and the answer that no class was decided.
const (
	Conflict   = "conflict"
	Fork       = "fork"
	Foreign    = "foreign"
	Unmeasured = "unmeasured"
	Own        = "own"
	Unreadable = "unreadable"
)

// Classes is the five classes in precedence order.
var Classes = []string{Conflict, Fork, Foreign, Unmeasured, Own}

// Signals is what was measured, and what could not be. Any entry in Unreadable stops
// classification: a signal that cannot be read is not a signal that agrees. A nil *bool is a
// signal nobody filled.
type Signals struct {
	EntryPointRecorded   bool
	DigestMatches        *bool
	HasPoint             bool
	RegistrationConflict string
	LinkConflict         string
	PointerConflict      string
	// Unlaunchable is what keeps a host from launching the entry point (a Go install's bin/crw
	// that is not an executable regular file, a compatibility link that does not resolve to it).
	Unlaunchable []string
	Unreadable   []string
}

// Classify returns (class, reasons); the first matching class wins.
func Classify(s Signals) (string, []string) {
	if len(s.Unreadable) > 0 {
		reasons := make([]string, len(s.Unreadable))
		for i, what := range s.Unreadable {
			reasons[i] = "classification stopped: " + what + " could not be read"
		}
		return Unreadable, reasons
	}
	var reasons []string
	for _, conflict := range []string{s.RegistrationConflict, s.LinkConflict, s.PointerConflict} {
		if conflict != "" {
			reasons = append(reasons, conflict)
		}
	}
	if len(reasons) > 0 {
		return Conflict, reasons
	}
	if len(s.Unlaunchable) > 0 {
		// runtime_install.resolve_entry_point finds an entry point with shutil.which, which
		// answers only a file the user may execute: one nothing can launch is found nowhere,
		// and Python classifies the component foreign. Its bytes do not change that.
		reasons = append(reasons, s.Unlaunchable...)
		return Foreign, append(reasons, "nothing a host starts through the entry point can run, so it is not this installation's")
	}
	differs := s.DigestMatches != nil && !*s.DigestMatches
	if s.EntryPointRecorded && differs {
		return Fork, []string{"the installed bytes differ from the recorded digest"}
	}
	if !s.EntryPointRecorded {
		reasons = append(reasons, "the entry point resolves outside every recorded path")
		if differs {
			reasons = append(reasons, "its bytes also differ from the recorded digest, so it is unverified")
		} else if !s.HasPoint {
			reasons = append(reasons, "no recorded run covers the combination it runs under, so it is unmeasured")
		}
		return Foreign, reasons
	}
	if !s.HasPoint {
		return Unmeasured, []string{"everything about the installed bytes agrees, but no recorded run covers the combination it runs under, so it is preserved and not reused"}
	}
	return Own, []string{"entry point, digest and a measured point all agree"}
}

// Reusable reports that only 'own' may be reused for a host-required command.
func Reusable(class string) bool { return class == Own }
