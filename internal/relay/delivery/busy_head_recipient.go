package delivery

import "context"

// BusyHeadHoldsRecipientLine reports whether a delivery to this recipient is waiting out a busy
// backoff at now: the head busyHeadSQL names for that recipient, which is the row eligibility lists
// by, behindBusyHead asks about and the claim refuses a younger delivery behind.
//
// It is that same predicate asked of a recipient that has no delivery of its own, so a caller
// outside the delivery path - the notice channel, I-216 - cannot disagree with it about which
// deliveries hold a line. It reads on ctx's querier, so a caller inside a write transaction asks it
// under that transaction's lock.
func (d *Service) BusyHeadHoldsRecipientLine(ctx context.Context, recipient string, now float64) (bool, error) {
	row, err := one(ctx, d.Store, "SELECT 1 AS held FROM "+busyHeadSQL+" bh WHERE bh.recipient_task_id = ?", append(d.busyHeadArgs(now), recipient)...)
	return row != nil, err
}
