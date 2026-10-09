package store

import (
	"errors"
	"fmt"
	"testing"
)

// CRW-945: the two error carriers the halt path uses. They change no message and hide no failure from the
// classifier.

func TestSiteError_carriesTheSiteAndKeepsTheTextAndTheClass(t *testing.T) {
	t.Parallel()
	base := fmt.Errorf("transaction body: %w", errors.New("plain failure"))
	marked := AtSite(HaltSiteWrite, base)
	if marked.Error() != base.Error() || !errors.Is(marked, base) {
		t.Fatalf("the marked error changed the failure: %q", marked)
	}
	if got := SiteOf(fmt.Errorf("wrapped: %w", marked), HaltSiteObservation); got != HaltSiteWrite {
		t.Fatalf("site %q", got)
	}
	if got := SiteOf(base, HaltSiteObservation); got != HaltSiteObservation {
		t.Fatalf("an unmarked error answered %q instead of the fallback", got)
	}
	if AtSite(HaltSiteWrite, nil) != nil {
		t.Fatal("a nil error was marked")
	}
}

func TestDetectedCorruption_isTheCorruptingClassWhateverItWraps(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("sweep: %w", &DetectedCorruption{Cause: CorruptingCause{Code: 522, Message: "disk I/O error"}, Err: errors.New("the marker could not be written")})
	cause, ok := CorruptingFailure(err)
	if !ok || cause.Code != 522 || cause.Message != "disk I/O error" {
		t.Fatalf("%+v, %v", cause, ok)
	}
	if _, ok := CorruptingFailure(errors.New("plain failure")); ok {
		t.Fatal("a plain failure was classified")
	}
}
