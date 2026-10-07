package adapter

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// The idle edge of a busy backoff (CRW-904, section 82 of the CRW-781 decision): while a delivery
// to a recipient waits out a busy backoff, the relay keeps a subscription on that recipient and the
// App Server's thread/status/changed to idle (or notLoaded) makes the head due at once. The relay's
// connection is what has to be subscribed, so these three calls live beside the transport they use.

// idleStatusParams is the params of one thread/status/changed notification. The status is read as
// either an object carrying a type or a bare string, so a host that reports either form is read the
// same way; anything else leaves the report without a status, which wakes nothing.
type idleStatusParams struct {
	ThreadID string          `json:"threadId"`
	Status   json.RawMessage `json:"status"`
}

func idleStatusOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.Type
	}
	return ""
}

// IdleReports reads the thread status reports this connection received since the last read, in
// order. It never blocks: the client's notification buffer is bounded at 64 and drops when the
// relay cannot keep up, which leaves the doubling backoff as the trigger for a report it never
// saw. A transport that is not the App Server client reports none, and so does an adapter whose
// RPC was replaced by a scripted double.
func (a *Adapter) IdleReports() []delivery.IdleReport {
	client, ok := a.rpc.(*appserver.Client)
	if !ok {
		return nil
	}
	var out []delivery.IdleReport
	for {
		select {
		case note, open := <-client.Notifications():
			if !open {
				return out
			}
			if note.Method != "thread/status/changed" {
				continue
			}
			var params idleStatusParams
			if json.Unmarshal(note.Params, &params) != nil || params.ThreadID == "" {
				continue
			}
			out = append(out, delivery.IdleReport{ThreadID: params.ThreadID, Status: idleStatusOf(params.Status)})
		default:
			return out
		}
	}
}

// HoldThread subscribes this relay's connection to a recipient thread and keeps the subscription
// without an admitted watch (CRW-904). The call is thread/resume with no overrides and no settings:
// on an already loaded thread it reports the state and starts no turn, so a busy recipient is never
// interrupted (I-30). A refusal is returned as it is, and the busy backoff stays the only trigger.
func (a *Adapter) HoldThread(ctx context.Context, thread string) error {
	client, ok := a.rpc.(*appserver.Client)
	if !ok {
		return errors.New("this transport has no App Server subscription to hold")
	}
	ctx, release, err := a.admitRead(ctx)
	if err != nil {
		return err
	}
	defer release()
	return client.HoldThread(ctx, thread)
}

// ReleaseThread drops the hold HoldThread took: the recipient's backlog emptied or its delivery was
// delivered. The bridge's own retention and any live watch are left alone.
func (a *Adapter) ReleaseThread(thread string) {
	if client, ok := a.rpc.(*appserver.Client); ok {
		client.ReleaseThread(thread)
	}
}

// ThreadSubscribed reports whether this connection already holds a subscription on the thread, so
// the relay does not open a second one: the reports it would receive are the same reports.
func (a *Adapter) ThreadSubscribed(thread string) bool {
	if client, ok := a.rpc.(*appserver.Client); ok {
		return client.ThreadSubscribed(thread)
	}
	return false
}
