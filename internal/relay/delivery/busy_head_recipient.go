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

// BusyHeadRecipients names every recipient whose line a delivery holds at now: one entry per
// recipient busyHeadSQL names a head for, which is the same rule BusyHeadHoldsRecipientLine asks of
// one recipient, asked once for all of them. A caller that decides an order across recipients at
// once - the supervisor channel's head selection, I-216's notice part - needs the whole set before
// it can leave the notices those lines hold out of that order, and asking the one-recipient
// predicate once per candidate would ask the same derived table again for each. It reads on ctx's
// querier, so a caller inside a write transaction asks it under that transaction's lock.
func (d *Service) BusyHeadRecipients(ctx context.Context, now float64) ([]string, error) {
	rows, err := all(ctx, d.Store, "SELECT bh.recipient_task_id AS recipient_task_id FROM "+busyHeadSQL+" bh", d.busyHeadArgs(now)...)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.S("recipient_task_id"))
	}
	return out, nil
}
