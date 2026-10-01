package delivery

import (
	"context"
	"database/sql"
	"slices"
	"sort"
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
	Delivery *Service
	Ack      *Ack
	// MaxSendsTick is policy.max_sends_per_tick: 0 means its default 4, a negative value 0. It is
	// the tick's budget of attempts, whatever their outcome.
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
	return sc.setCursor(ctx, listing, strconv.Itoa(position))
}

func (sc *Scheduler) setCursor(ctx context.Context, listing, value string) error {
	return sc.Delivery.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, sc.Delivery.Store, "INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at) VALUES ('scheduler',?,?,?) ON CONFLICT(task_id, listing) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at", listing, value, sc.Delivery.Clock.ISO())
		return err
	})
}

// pointer is a cursor that holds a key rather than a position: "" when none is stored. A
// position into a list that changes between ticks drifts when rows leave and come back; a key
// does not, because the walk resumes at the first entry after it.
func (sc *Scheduler) pointer(ctx context.Context, listing string) (string, error) {
	row, err := one(ctx, sc.Delivery.Store, "SELECT cursor FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = ?", listing)
	if err != nil || row == nil {
		return "", err
	}
	return row.S("cursor"), nil
}

func (sc *Scheduler) clearPointer(ctx context.Context, listing string) error {
	return sc.Delivery.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, sc.Delivery.Store, "DELETE FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = ?", listing)
		return err
	})
}

// rowKey is the place of a delivery in event creation order, as one string that compares the way
// eligibleOrder sorts.
func rowKey(r Row) string {
	return r.S("first_seen_at") + "|" + r.S("created_at") + "|" + r.S("event_id")
}

// queue is one recipient's due deliveries of one parent, in the order this tick walks them.
type queue struct {
	recipient string
	// rows start after the marker and wrap once, so a tick with no marker walks creation order.
	rows []Row
	// marker is the key of the row the previous tick refused last, "" when it refused none.
	marker string
	next   int
	// refused is the key of the last row this tick refused.
	refused   string
	attempted bool
	done      bool
}

// parentWalk is one parent's turn in a tick: up to MaxSendsPerParentPerTick recipient queues, taken
// round robin by recipient id from the one after the recipient last attempted, so a recipient that
// cannot take a send never keeps another recipient of the same parent waiting.
type parentWalk struct {
	parent   string
	share    int
	queues   []*queue
	current  int
	attempts int
	last     string
}

func pointerKey(parent string) string           { return "deliver:" + parent }
func markerKey(parent, recipient string) string { return "deliver:" + parent + ">" + recipient }

// open builds a parent's walk from its due rows. It returns nil when none are due.
func (sc *Scheduler) open(ctx context.Context, parent string, now float64, share int) (*parentWalk, error) {
	rows, err := sc.Delivery.EligibleRows(ctx, parent, now)
	if err != nil || len(rows) == 0 || share <= 0 {
		return nil, err
	}
	byRecipient := map[string][]Row{}
	var recipients []string
	for _, r := range rows {
		id := r.S("recipient_task_id")
		if _, seen := byRecipient[id]; !seen {
			recipients = append(recipients, id)
		}
		byRecipient[id] = append(byRecipient[id], r)
	}
	sort.Strings(recipients)
	after, err := sc.pointer(ctx, pointerKey(parent))
	if err != nil {
		return nil, err
	}
	// The first recipient after the pointer, wrapping; a legacy integer pointer sorts before or
	// after every id and the walk simply starts at the first queue.
	start := sort.Search(len(recipients), func(i int) bool { return recipients[i] > after })
	w := &parentWalk{parent: parent, share: share}
	for i := 0; i < len(recipients) && len(w.queues) < share; i++ {
		id := recipients[(start+i)%len(recipients)]
		marker, err := sc.pointer(ctx, markerKey(parent, id))
		if err != nil {
			return nil, err
		}
		w.queues = append(w.queues, &queue{recipient: id, rows: afterMarker(byRecipient[id], marker), marker: marker})
	}
	return w, nil
}

// afterMarker puts the rows after the marker first and the rest, wrapped, behind them.
func afterMarker(rows []Row, marker string) []Row {
	if marker == "" {
		return rows
	}
	split := sort.Search(len(rows), func(i int) bool { return rowKey(rows[i]) > marker })
	return append(slices.Clone(rows[split:]), rows[:split]...)
}

// Deliver is daemon._deliver: a fair slice, one struggling parent never spending the budget.
//
// The tick has one budget of attempts (MaxSendsTick) and each parent may use at most
// MaxSendsPerParentPerTick of it; parents take turns one attempt at a time, in the order the
// delivery_parents cursor rotates. Within a parent the queue of a recipient goes oldest event
// first. An attempt that sends, finds the recipient busy, withholds, or is refused by the
// recipient's gap ends that recipient's queue for the tick. An attempt that is refused outright (it
// returned an error, or nothing while its row did not move) does not: the next row of that
// recipient is tried, within the budget, and the refused row's key is kept as a marker so the next
// tick starts after it and a row that cannot be sent never holds back the rows behind it. A tick
// that refuses nothing clears the marker, so with nothing refused a recipient's rows go out in
// creation order.
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
	budget := sc.MaxSendsTick
	if budget == 0 {
		budget = 4
	}
	if budget < 0 {
		budget = 0
	}
	order := append(slices.Clone(parents[cursor:]), parents[:cursor]...)
	if err := sc.advance(ctx, "delivery_parents", 1, len(parents)); err != nil {
		return err
	}
	var walks []*parentWalk
	for _, parent := range order {
		w, err := sc.open(ctx, parent, now, d.Policy.MaxSendsPerParentPerTick)
		if err != nil {
			return err
		}
		if w != nil {
			walks = append(walks, w)
		}
	}
	for budget > 0 {
		progressed := false
		for _, w := range walks {
			if budget == 0 {
				break
			}
			row, q := w.next()
			if row == nil {
				continue
			}
			budget--
			progressed = true
			if sc.attempt(ctx, adapter, row, now, report) {
				q.refused = rowKey(row)
			} else {
				q.done = true
			}
			if q.next >= len(q.rows) {
				q.done = true
			}
		}
		if !progressed {
			break
		}
	}
	for _, w := range walks {
		if err := sc.close(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// next is the walk's next row and the queue it belongs to: the current queue's next row, moving
// on to the next queue when one is done. It is nil when the parent's attempts or queues are spent.
func (w *parentWalk) next() (Row, *queue) {
	if w.attempts >= w.share {
		return nil, nil
	}
	for w.current < len(w.queues) && w.queues[w.current].done {
		w.current++
	}
	if w.current >= len(w.queues) {
		return nil, nil
	}
	q := w.queues[w.current]
	row := q.rows[q.next]
	q.next++
	q.attempted = true
	w.attempts++
	w.last = q.recipient
	return row, q
}

// close stores what the tick learned: the recipient last attempted, and for each queue attempted
// the key of its last refused row, or nothing when it refused none. A queue not attempted keeps
// its marker.
func (sc *Scheduler) close(ctx context.Context, w *parentWalk) error {
	if w.last == "" {
		return nil
	}
	if err := sc.setCursor(ctx, pointerKey(w.parent), w.last); err != nil {
		return err
	}
	for _, q := range w.queues {
		switch {
		case !q.attempted:
		case q.refused != "":
			if err := sc.setCursor(ctx, markerKey(w.parent, q.recipient), q.refused); err != nil {
				return err
			}
		case q.marker != "":
			if err := sc.clearPointer(ctx, markerKey(w.parent, q.recipient)); err != nil {
				return err
			}
		}
	}
	return nil
}

// attempt makes one attempt and counts it. It reports true when the row was refused outright and
// the recipient's next row is still worth trying this tick.
func (sc *Scheduler) attempt(ctx context.Context, adapter Adapter, row Row, now float64, report *TickCounts) bool {
	d := sc.Delivery
	event := row.S("event_id")
	record, err := d.Attempt(ctx, event, adapter, &now, "")
	if err != nil {
		report.Notes = append(report.Notes, "delivery refused for "+event+": "+err.Error())
		return true
	}
	if record == nil {
		report.Deferred++
		moved, err := d.moved(ctx, row)
		if err != nil {
			report.Notes = append(report.Notes, "delivery "+event+" not re-read: "+err.Error())
			return false
		}
		return !moved
	}
	if v, _ := get(record, "withheldReason"); truthy(v) {
		report.Deferred++
		return false
	}
	if str(record, "sendAttempted") == "no" {
		report.Skipped++
	} else {
		report.Delivered++
	}
	if row.S("kind") == Revision && str(record, "deliveryState") == Dispatched && sc.Ack != nil {
		if _, err := sc.Ack.BindDispatchedRevision(ctx, event); err != nil {
			report.Notes = append(report.Notes, "anchor binding failed: "+err.Error())
		}
	}
	return false
}

// moved reports whether a delivery changed its own scheduling since it was selected: its state, its
// attempt count, its backoff or a hold. A row that was attempted and did not move is stuck, and
// the scheduler does not let it keep the rows behind it waiting.
func (d *Service) moved(ctx context.Context, selected Row) (bool, error) {
	current, err := d.Find(ctx, selected.S("event_id"))
	if err != nil || current == nil {
		return true, err
	}
	return current.S("state") != selected.S("state") || current.I("attempt_count") != selected.I("attempt_count") ||
		current.Opt("next_eligible_at") != selected.Opt("next_eligible_at") || !current.N("hold_reason"), nil
}
