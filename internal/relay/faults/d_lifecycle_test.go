package faults

import "testing"

// Clock and entropy are inputs on both sides, never output substitutions.
func TestDNotificationLifecycleWholeReplies(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	check := func(args ...string) map[string]any { t.Helper(); return f1ReplayCLI(t, ctx, gd, args) }
	observed := check("fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`)
	check("fault-notification-raise", "--fault", observed["faultId"].(string), "--reason", "classification")
	check("fault-notifications")
	reserve := func() map[string]any {
		t.Helper()
		return check("fault-notification-reserve", "--owner", "worker", "--limit", "1")["reserved"].([]any)[0].(map[string]any)
	}
	one := reserve()
	check("fault-notification-fail", "--notification", one["notificationId"].(string), "--token", one["token"].(string), "--error", "not sent")
	one = reserve()
	notice := one["notificationId"].(string)
	check("fault-notification-ack", "--notification", notice, "--token", one["token"].(string), "--ref", "sent")
	one = reserve()
	uncertain := one["notificationId"].(string)
	f1Seed(t, ctx, gd, []string{"UPDATE fault_notifications SET lease_until=0 WHERE notification_id='" + uncertain + "'"})
	check("fault-notifications", "--notification-state", "uncertain")
	check("fault-notification-reconcile", "--notification", uncertain, "--delivered", "no", "--ref", "not sent")
	one = reserve()
	arrived := one["notificationId"].(string)
	f1Seed(t, ctx, gd, []string{"UPDATE fault_notifications SET lease_until=0 WHERE notification_id='" + arrived + "'"})
	check("fault-notification-reconcile", "--notification", arrived, "--delivered", "yes", "--ref", "confirmed")
	check("fault-notifications", "--notification-state", "delivered")
	check("fault-notification-reconcile", "--notification", notice, "--delivered", "no", "--ref", "none")
}
