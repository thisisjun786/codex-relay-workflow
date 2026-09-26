package delivery

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
)

// TickCounts are the delivery counters of the daemon's TickReport.
type TickCounts struct {
	Delivered, Deferred, Skipped int
	Notes                        []string
}

// Scheduler is the delivery pass of RelayDaemon.tick (daemon._deliver), with its persisted
// rotation cursors. The daemon (todo 29) owns the rest of the tick and calls this pass.
type Scheduler struct {
	Delivery     *Service
	Ack          *Ack
	MaxSendsTick int
}

func (sc *Scheduler) cursor(ctx context.Context, listing string, size int) (int, error) {
	if size <= 0 {
		return 0, nil
	}
	row, err := one(ctx, sc.Delivery.Store, "SELECT cursor FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = ?", listing)
	if err != nil || row == nil || row.N("cursor") {
		return 0, err
	}
	n, convErr := strconv.Atoi(row.S("cursor"))
	if convErr != nil {
		return 0, nil
	}
	return n % size, nil
}

func (sc *Scheduler) advance(ctx context.Context, listing string, by, size int) error {
	if size <= 0 {
		return nil
	}
	current, err := sc.cursor(ctx, listing, size)
	if err != nil {
		return err
	}
	position := (current + max(1, by)) % size
	return sc.Delivery.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, sc.Delivery.Store, "INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at) VALUES ('scheduler',?,?,?) ON CONFLICT(task_id, listing) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at", listing, strconv.Itoa(position), sc.Delivery.Clock.ISO())
		return err
	})
}

// Deliver is daemon._deliver: a fair slice, one struggling parent never spending the budget.
func (sc *Scheduler) Deliver(ctx context.Context, adapter Adapter, now float64, report *TickCounts) error {
	d := sc.Delivery
	parents, err := d.EligibleParents(ctx, now)
	if err != nil || len(parents) == 0 {
		return err
	}
	cursor, err := sc.cursor(ctx, "delivery_parents", len(parents))
	if err != nil {
		return err
	}
	totals := map[string]int{}
	offsets := map[string]int{}
	for _, p := range parents {
		n, err := d.EligibleCount(ctx, p, now)
		if err != nil {
			return err
		}
		totals[p] = int(n)
		if n > 0 {
			if offsets[p], err = sc.cursor(ctx, "deliver:"+p, int(n)); err != nil {
				return err
			}
		}
	}
	limit := sc.MaxSendsTick
	if limit == 0 {
		limit = 4
	}
	eligible, err := d.Eligible(ctx, now, limit, 0, cursor, offsets)
	if err != nil {
		return err
	}
	if err := sc.advance(ctx, "delivery_parents", 1, len(parents)); err != nil {
		return err
	}
	struggling := map[string]bool{}
	attempted := map[string]int{}
	var order []string
	for _, row := range eligible {
		p := row.S("parent_task_id")
		if struggling[p] {
			report.Skipped++
			continue
		}
		if !slices.Contains(order, p) {
			order = append(order, p)
		}
		attempted[p]++
		record, err := d.Attempt(ctx, row.S("event_id"), adapter, &now, "")
		if err != nil {
			report.Notes = append(report.Notes, "delivery refused for "+row.S("event_id")+": "+err.Error())
			struggling[p] = true
			continue
		}
		if record == nil {
			struggling[p] = true
			report.Deferred++
			continue
		}
		state := str(record, "deliveryState")
		if state == HeldUncertain || state == DeferredBusy || state == WithheldPreSend {
			struggling[p] = true
		}
		if v, _ := get(record, "withheldReason"); truthy(v) {
			report.Deferred++
			continue
		}
		if str(record, "sendAttempted") == "no" {
			report.Skipped++
		} else {
			report.Delivered++
		}
		if row.S("kind") == Revision && state == Dispatched && sc.Ack != nil {
			if _, err := sc.Ack.BindDispatchedRevision(ctx, row.S("event_id")); err != nil {
				report.Notes = append(report.Notes, "anchor binding failed: "+err.Error())
			}
		}
	}
	for _, p := range order {
		if totals[p] > 0 {
			if err := sc.advance(ctx, "deliver:"+p, attempted[p], totals[p]); err != nil {
				return err
			}
		}
	}
	return nil
}
