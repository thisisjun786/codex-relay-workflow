package delivery

import "math"

// Hold and pacing words (policy.py).
const (
	AttemptCap             = "attempt_cap"
	BusyCap                = "busy_cap"
	PushChannelClosed      = "push_channel_closed"
	SupersededHold         = "superseded"
	HostLostTurn           = "host_lost_turn"
	TurnCheckUndecided     = "turn_check_undecided"
	UnknownSendLost        = "unknown_send_lost"
	UnknownSendUndecided   = "unknown_send_undecided"
	UnknownSendHoldNamed   = "unknown_send_hold_named"
	MinSendInterval        = "min_send_interval"
	HourlyCap              = "hourly_cap"
	RateWindowSeconds      = 3600.0
	RecipientUndeliverable = "recipient_undeliverable"
)

// RetryPolicy is policy.RetryPolicy with its defaults.
type RetryPolicy struct {
	BusyBase, BusyMax           float64
	BusyMaxAttempts             int64
	PresendBase, PresendMax     float64
	MaxAttempts                 int64
	MinSendInterval             float64
	MaxSendsPerRecipientPerHour int64
	LifecycleRecheck            float64
	Lease                       float64
	MaxSendsPerParentPerTick    int
}

func DefaultPolicy() RetryPolicy {
	return RetryPolicy{BusyBase: 15, BusyMax: 300, BusyMaxAttempts: 40, PresendBase: 30, PresendMax: 900, MaxAttempts: 6,
		MinSendInterval: 5, MaxSendsPerRecipientPerHour: 12, LifecycleRecheck: 60, Lease: 300, MaxSendsPerParentPerTick: 2}
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

// Pacing is policy.pacing: why a recipient may not be woken now, or nil.
func (p RetryPolicy) Pacing(now float64, sends int64, last *float64) Obj {
	window, _ := p.RateWindows(now)
	capacity := p.MaxSendsPerRecipientPerHour
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
