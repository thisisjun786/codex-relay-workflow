package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// busMethods are the notifications the bus carries per thread. Each has a top-level threadId in the
// 0.154.0 protocol. Every other notification stays on Client.Notifications() alone.
var busMethods = map[string]bool{
	"turn/completed":            true,
	"item/completed":            true,
	"thread/tokenUsage/updated": true,
	"thread/status/changed":     true,
	"error":                     true,
}

// DefaultFeedBuffer is how many notifications a feed holds before it drops the oldest.
const DefaultFeedBuffer = 256

// reconcilePageLimit bounds the pages one ReconcileTurns call reads of a thread's turns or of one
// turn's items; a host that never ends a list is an error, not an endless loop.
const reconcilePageLimit = 1000

// ThreadID is the threadId member of the notification's params, or "" when it has none.
func (n Notification) ThreadID() string {
	var params struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(n.Params, &params)
	return params.ThreadID
}

// Bus hands each thread's notifications to the consumer that follows that thread. It sits beside
// Client.Notifications(), which stays the single unfiltered channel it always was: the receive loop
// offers every notification to both, so adding a bus changes nothing for an existing reader.
type Bus struct {
	client *Client
	mu     sync.Mutex
	feeds  map[string][]*Feed
}

func newBus(c *Client) *Bus { return &Bus{client: c, feeds: map[string][]*Feed{}} }

// Bus is the client's notification bus.
func (c *Client) Bus() *Bus { return c.bus }

// Feed is one consumer's view of one thread's notifications.
type Feed struct {
	bus     *Bus
	thread  string
	events  chan Notification
	lost    chan struct{}
	mu      sync.Mutex
	closed  bool
	dropped uint64
}

// Subscribe follows thread with a feed of DefaultFeedBuffer notifications.
func (b *Bus) Subscribe(thread string) *Feed { return b.SubscribeBuffered(thread, DefaultFeedBuffer) }

// SubscribeBuffered follows thread with a feed that holds at most size notifications.
func (b *Bus) SubscribeBuffered(thread string, size int) *Feed {
	if size < 1 {
		size = 1
	}
	f := &Feed{bus: b, thread: thread, events: make(chan Notification, size), lost: make(chan struct{}, 1)}
	b.mu.Lock()
	b.feeds[thread] = append(b.feeds[thread], f)
	b.mu.Unlock()
	return f
}

// Notifications delivers this thread's notifications in the order the host sent them. The channel
// is closed by Close.
func (f *Feed) Notifications() <-chan Notification { return f.events }

// Lost signals that this feed may have missed notifications the host sent: its connection ended, or
// the consumer fell behind and the oldest notifications were dropped. The consumer reads the turns
// it is waiting for (Bus.ReconcileTurns) instead of waiting for a notification that may never come.
//
// A feed only receives what its client's connection receives, and the host sends a thread's
// notifications to the connections subscribed to it. After a loss the client dials a new connection
// that is subscribed to nothing: reading turns does not subscribe it (reads do not, see
// docs/relay/subscriptions.md). A consumer that finds a turn still inProgress therefore resumes the
// thread on the new connection (thread/resume) before it goes back to waiting on this feed.
func (f *Feed) Lost() <-chan struct{} { return f.lost }

// Dropped counts the notifications dropped because the consumer fell behind.
func (f *Feed) Dropped() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}

// Close ends the feed. It is safe to call twice.
func (f *Feed) Close() {
	b := f.bus
	b.mu.Lock()
	list := b.feeds[f.thread]
	for i, other := range list {
		if other == f {
			b.feeds[f.thread] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(b.feeds[f.thread]) == 0 {
		delete(b.feeds, f.thread)
	}
	b.mu.Unlock()
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	f.mu.Unlock()
}

func (f *Feed) signalLost() {
	select {
	case f.lost <- struct{}{}:
	default:
	}
}

// deliver offers one notification; a full feed drops its oldest so the newest, which carries the
// terminal state, is kept.
func (f *Feed) deliver(n Notification) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	for {
		select {
		case f.events <- n:
			return
		default:
		}
		select {
		case <-f.events:
			f.dropped++
			f.signalLost()
		default:
		}
	}
}

// publish routes a notification to the feeds of its thread. It runs on the connection's reader and
// never blocks.
func (b *Bus) publish(method string, params json.RawMessage) {
	if b == nil {
		return
	}
	if !busMethods[method] {
		return
	}
	n := Notification{Method: method, Params: params}
	thread := n.ThreadID()
	if thread == "" {
		return
	}
	b.mu.Lock()
	feeds := append([]*Feed(nil), b.feeds[thread]...)
	b.mu.Unlock()
	for _, f := range feeds {
		f.deliver(n)
	}
}

// lost tells every feed that the connection that carried its notifications ended.
func (b *Bus) lost() {
	if b == nil {
		return
	}
	b.mu.Lock()
	var all []*Feed
	for _, list := range b.feeds {
		all = append(all, list...)
	}
	b.mu.Unlock()
	for _, f := range all {
		f.signalLost()
	}
}

// TurnReading is what the host holds for one turn id.
type TurnReading struct {
	TurnID string
	// Found is whether thread/turns/list returned the turn.
	Found bool
	// Status is the turn's status (completed, interrupted, failed, inProgress).
	Status string
	// Error is the turn's error member verbatim when it has one.
	Error json.RawMessage
	// Items are the turn's thread items in full. They are meaningful only when Readable.
	Items []json.RawMessage
	// Readable is whether Items is the turn's complete item list. A turn whose items could not be read
	// is unreadable, which is not the same as a turn with no items.
	Readable bool
	// ReadError says why a found turn is not Readable.
	ReadError error
}

type turnsPage struct {
	Data []struct {
		ID        string            `json:"id"`
		Status    string            `json:"status"`
		Error     json.RawMessage   `json:"error"`
		ItemsView string            `json:"itemsView"`
		Items     []json.RawMessage `json:"items"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

type itemsPage struct {
	Data []struct {
		Item json.RawMessage `json:"item"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

// ReconcileTurns reads the turns named by turnIDs of thread by their exact ids, in the order given.
// It pages thread/turns/list with itemsView "full" until every id is found (or the list ends), and
// reads the items of a turn the list did not return in full with thread/items/list. It never asks
// for includeTurns hydration. It only reads: it does not subscribe the connection it reads on, so a
// reading of inProgress is a state at that moment, and a live completion is delivered to the feed only
// after the caller resumes the thread on that connection (see Feed.Lost). An error is returned when the host refuses the list itself; a turn
// whose items cannot be read is reported as found and not Readable.
func (b *Bus) ReconcileTurns(ctx context.Context, thread string, turnIDs []string) ([]TurnReading, error) {
	readings := make([]TurnReading, len(turnIDs))
	index := map[string][]int{}
	for i, id := range turnIDs {
		readings[i] = TurnReading{TurnID: id}
		index[id] = append(index[id], i)
	}
	remaining := len(index)
	needItems := map[string]bool{}
	cursor := ""
	for page := 0; remaining > 0; page++ {
		if page >= reconcilePageLimit {
			return nil, fmt.Errorf("thread/turns/list: more than %d pages", reconcilePageLimit)
		}
		params := map[string]any{"threadId": thread, "itemsView": "full"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := b.client.Call(ctx, "thread/turns/list", params)
		if err != nil {
			return nil, err
		}
		var decoded turnsPage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("thread/turns/list: %w", err)
		}
		for _, turn := range decoded.Data {
			positions, wanted := index[turn.ID]
			if !wanted || readings[positions[0]].Found {
				continue
			}
			remaining--
			for _, i := range positions {
				r := &readings[i]
				r.Found, r.Status = true, turn.Status
				if len(turn.Error) > 0 && string(turn.Error) != "null" {
					r.Error = turn.Error
				}
				if turn.ItemsView == "full" || turn.ItemsView == "" {
					r.Items, r.Readable = turn.Items, true
				} else {
					needItems[turn.ID] = true
				}
			}
		}
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			break
		}
		cursor = *decoded.NextCursor
	}
	for id := range needItems {
		items, err := b.turnItems(ctx, thread, id)
		for _, i := range index[id] {
			if err != nil {
				readings[i].ReadError = err
			} else {
				readings[i].Items, readings[i].Readable = items, true
			}
		}
	}
	return readings, nil
}

// turnItems pages thread/items/list for one turn.
func (b *Bus) turnItems(ctx context.Context, thread, turn string) ([]json.RawMessage, error) {
	items := []json.RawMessage{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= reconcilePageLimit {
			return nil, errors.New("thread/items/list: more pages than the bound allows")
		}
		params := map[string]any{"threadId": thread, "turnId": turn}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := b.client.Call(ctx, "thread/items/list", params)
		if err != nil {
			return nil, err
		}
		var decoded itemsPage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("thread/items/list: %w", err)
		}
		for _, entry := range decoded.Data {
			items = append(items, entry.Item)
		}
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			return items, nil
		}
		cursor = *decoded.NextCursor
	}
}
