package faults

import "testing"

func TestDNotificationRelationshipEligibilityAgainstPython(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	check := func(args ...string) map[string]any { t.Helper(); return f1ReplayCLI(t, ctx, gd, pd, args) }
	answer := check("fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"observation_unmeasured","severity":"notice","signature":{"relationship":"rel","turn":"turn-1"},"occurrenceKey":"u1","scope":{"projectKey":"CRW"}}`)
	f1SeedBoth(t, ctx, gd, pd, []string{f1Relationship, "UPDATE relationships SET status='paused'"})
	check("fault-notification-raise", "--fault", answer["faultId"].(string), "--reason", "classification")
	check("fault-notifications")
	check("fault-notification-reserve", "--owner", "worker", "--limit", "1")
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE relationships SET status='active'"})
	check("fault-notifications")
	check("fault-notification-reserve", "--owner", "worker", "--limit", "1")
	// supervision.contactable's entire answer travels inside eligibility. Exercise
	// its fresh/stale/future boundaries, unreadable time, and negative observation.
	for _, tc := range []struct{ deliverable, stamp string }{
		{"yes", "1970-01-02T03:46:40+00:00"},
		{"yes", "1970-01-02T03:31:40+00:00"},
		{"yes", "1970-01-02T03:31:39+00:00"},
		{"yes", "1970-01-02T03:47:40+00:00"},
		{"yes", "1970-01-02T03:47:41+00:00"},
		{"yes", "unreadable"},
		{"no", "1970-01-02T03:46:40+00:00"},
	} {
		f1SeedBoth(t, ctx, gd, pd, []string{"DELETE FROM recipient_lifecycle", "INSERT INTO recipient_lifecycle(task_id,deliverable,observed_at) VALUES('parent','" + tc.deliverable + "','" + tc.stamp + "')"})
		check("fault-notifications")
	}
}
