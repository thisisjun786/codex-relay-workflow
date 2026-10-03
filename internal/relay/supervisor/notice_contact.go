package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// noticeContacts is the contact reading the fault notices use (supervision.contactable): whether the
// task above a fault can be told now, read from that task's last host observation. faults asks for it
// through faults.Contacts, wherever it reads eligibility.
//
// It is not Channel.contactability (selection.go), which answers the same question for a report and
// differs in what it prints: the age here is float64(UnixNano())/1e9 against that one's
// float64(UnixMicro())/1e6, which round differently on microsecond stamps, and the time layout and the
// columns read differ too. ageSeconds is part of what fault-notifications prints, so the two are kept
// as they were; the backlog entry for this move records the difference.
type noticeContacts struct{}

var _ faults.Contacts = noticeContacts{}

func init() { faults.SetContacts(noticeContacts{}) }

func (noticeContacts) Contactable(ctx context.Context, s *store.Store, parent string, now float64) (map[string]any, error) {
	var contact map[string]any
	if parent == "" {
		contact = map[string]any{"contactable": nil, "asked": false, "reason": "no recipient was named, so deliverability was not part of this question"}
	} else {
		lifecycle, e := s.One(ctx, "SELECT deliverable,withhold_reason,detail,observed_at FROM recipient_lifecycle WHERE task_id=?", parent)
		if e != nil {
			return nil, e
		}
		if lifecycle == nil {
			contact = map[string]any{"contactable": nil, "reason": "the host has not been observed for this task, so deliverability is unmeasured rather than allowed"}
		} else {
			deliverable, observed := lifecycle.Text("deliverable"), lifecycle.Text("observed_at")
			contact = map[string]any{"deliverable": deliverable, "observedAt": observed}
			if deliverable != "yes" {
				reason := lifecycle.Text("withhold_reason")
				if reason == "" {
					reason = deliverable
				}
				contact["contactable"] = false
				contact["reason"] = reason
			} else if stamp, parseErr := time.Parse("2006-01-02T15:04:05.999999Z07:00", observed); parseErr != nil {
				contact["contactable"] = nil
				contact["reason"] = "the observation carries no readable time, so its age is unmeasured"
			} else {
				age := now - float64(stamp.UnixNano())/1e9
				contact["ageSeconds"] = age
				switch {
				case age < -60:
					contact["contactable"] = nil
					contact["reason"] = fmt.Sprintf("the observation is dated %ds in the future, so it is not evidence about now", int(-age))
				case age > 900:
					contact["contactable"] = nil
					contact["reason"] = fmt.Sprintf("the observation is %ds old, past the 900s this reading treats as current", int(age))
				default:
					contact["contactable"] = true
					contact["reason"] = deliverable
				}
			}
		}
	}
	return contact, nil
}
