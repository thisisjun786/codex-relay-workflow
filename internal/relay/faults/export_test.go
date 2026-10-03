package faults

import "testing"

// realManagedObserver is the delivery-judgment observer the relay daemon installs in a Sweeper
// (supervisor.OmissionObserver). This package cannot import it (supervisor imports faults), so an external
// test file of the package installs it, as contacts_wiring_test.go installs the contact reading.
var realManagedObserver ManagedReadingObserver

// SetRealManagedObserver is called from the external test package that links the supervisor package.
func SetRealManagedObserver(o ManagedReadingObserver) { realManagedObserver = o }

// realObserver is the observer the daemon uses; a test binary that did not link it fails here.
func realObserver(t *testing.T) ManagedReadingObserver {
	t.Helper()
	if realManagedObserver == nil {
		t.Fatal("no managed observer is installed: managed_observer_wiring_test.go links the supervisor package")
	}
	return realManagedObserver
}
