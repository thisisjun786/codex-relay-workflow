package delivery

import "math"

// Hold and pacing words (policy.py).
const (
	AttemptCap           = "attempt_cap"
	BusyCap              = "busy_cap"
	PushChannelClosed    = "push_channel_closed"
	HostLostTurn         = "host_lost_turn"
	TurnCheckUndecided   = "turn_check_undecided"
	UnknownSendLost      = "unknown_send_lost"
	UnknownSendUndecided = "unknown_send_undecided"
	UnknownSendHoldNamed = "unknown_send_hold_named"
	MinSendInterval      = "min_send_interval"
	HourlyCap            = "hourly_cap"
	RateWindowSeconds    = 3600.0
)

// RetryPolicy is policy.RetryPolicy with its defaults.
//
// The send budget has two parts with two owners. MinSendInterval is the recipient's: a task is
// woken at most once in that gap, whoever sends. MaxSendsPerRelationshipPerHour is the
// relationship's: it fences one relationship (a parent and a child with one issue) from waking one
// recipient more than that many times in an hour, which is what a loop looks like. The cap used to
// be counted per recipient, so every child of a parent spent the same twelve and a parent with ten
// children stopped receiving; counted per relationship, a parent with R children can receive up to
// 12 R an hour (the gap still holds a recipient to 720 an hour) and one runaway relationship still
// stops at 12.
type RetryPolicy struct {
	BusyBase, BusyMax              float64
	BusyMaxAttempts                int64
	PresendBase, PresendMax        float64
	MaxAttempts                    int64
	MinSendInterval                float64
	MaxSendsPerRelationshipPerHour int64
	LifecycleRecheck               float64
	Lease                          float64
	// MaxSendsPerParentPerTick is how many recipients of one parent a tick attempts. A tick wakes a
	// recipient at most once (its instant is fixed and the gap is longer), so the count is of
	// recipients, not of queued rows. With the daemon's 20 s tick and four attempts a tick the
	// scheduler can deliver 180 an hour to a parent and 720 an hour in all.
	MaxSendsPerParentPerTick int
}

func DefaultPolicy() RetryPolicy {
	return RetryPolicy{BusyBase: 15, BusyMax: 300, BusyMaxAttempts: 40, PresendBase: 30, PresendMax: 900, MaxAttempts: 6,
		MinSendInterval: 5, MaxSendsPerRelationshipPerHour: 12, LifecycleRecheck: 60, Lease: 300, MaxSendsPerParentPerTick: 2}
}

func (p RetryPolicy) DelayFor(attemptNo int64, reason string) float64 {
	base, ceiling := p.PresendBase, p.PresendMax
	if reason == "busy" {
		base, ceiling = p.BusyBase, p.BusyMax
	}
	return math.Min(ceiling, base*math.Pow(2, float64(max(0, attemptNo-1))))
}

func (p RetryPolicy) CapFor(reason string) int64 {
	if reason == "busy" {
		return p.BusyMaxAttempts
	}
	return p.MaxAttempts
}

func (p RetryPolicy) CapReason(reason string) string {
	if reason == "busy" {
		return BusyCap
	}
	return AttemptCap
}

// RateWindows is the hour window of now and the earliest window the gap reaches.
func (p RetryPolicy) RateWindows(now float64) (float64, float64) {
	window := math.Floor(now/RateWindowSeconds) * RateWindowSeconds
	reach := RateWindowSeconds * (1 + math.Floor(p.MinSendInterval/RateWindowSeconds))
	return window, window - reach
}

// Pacing is policy.pacing: why a recipient may not be woken now, or nil. sends is how many sends
// the delivery's own relationship has charged to the recipient this hour, and last is the
// recipient's latest send of any relationship.
func (p RetryPolicy) Pacing(now float64, sends int64, last *float64) Obj {
	window, _ := p.RateWindows(now)
	capacity := p.MaxSendsPerRelationshipPerHour
	var gapEnds *float64
	if last != nil {
		g := *last + p.MinSendInterval
		gapEnds = &g
	}
	reading := Obj{{Key: "sends", Value: sends}, {Key: "cap", Value: capacity}, {Key: "windowStart", Value: window}}
	if sends >= capacity {
		if capacity <= 0 {
			return append(Obj{{Key: "reason", Value: HourlyCap}, {Key: "reopensAt", Value: nil}}, append(reading,
				F{Key: "detail", Value: "a cap of zero refuses every send; only a changed policy reopens it"})...)
		}
		reopens := window + RateWindowSeconds
		if gapEnds != nil && *gapEnds > reopens {
			reopens = *gapEnds
		}
		return append(Obj{{Key: "reason", Value: HourlyCap}, {Key: "reopensAt", Value: reopens}}, reading...)
	}
	if gapEnds != nil && now < *gapEnds {
		return append(Obj{{Key: "reason", Value: MinSendInterval}, {Key: "reopensAt", Value: *gapEnds}}, reading...)
	}
	return nil
}
