package integrator

import (
	"context"
)

// DryEntry is one pending head as the dry run sees it.
type DryEntry struct {
	Entry
	// InCheckout says the commit exists in the checkout; AlreadyInDev says it is an ancestor of
	// the local remote-tracking ref (not fetched, so as of the last fetch).
	InCheckout   bool   `json:"in_checkout"`
	AlreadyInDev bool   `json:"already_in_dev"`
	Gate         string `json:"gate,omitempty"`
	Retried      bool   `json:"retried"`
	// Landed says why a pull request entry already landed at its head (done/ or its landing record): the pass moves it to done/
	// with no run, so it is not planned.
	Landed string `json:"landed,omitempty"`
}

// DryReport is what a dry run reports.
type DryReport struct {
	Stop      bool       `json:"stop_file"`
	Lock      string     `json:"lock"`
	Base      string     `json:"base"`
	SoloHeads []string   `json:"solo_heads,omitempty"`
	Entries   []DryEntry `json:"entries"`
	Ignored   []string   `json:"ignored,omitempty"`
	// Claims are files an earlier pass could neither move on nor put back into pending; the next
	// pass puts them back before it reads the queue.
	Claims []string `json:"claims,omitempty"`
	// Plan is the specs the next pass would hand to one integration run, in order, when every
	// gate passes (the gate is only consulted with withGate).
	Plan []string `json:"plan"`
	// BatchRetried says a retry marker exists for the planned batch.
	BatchRetried bool `json:"batch_retried"`
}

// DryRun reads the queue and the checkout and changes nothing: no directory is made, no file
// moved, no lock taken (it is only probed), no fetch. The gate command runs only with
// withGate, because a gate may fetch on its own.
func (r *Runner) DryRun(ctx context.Context, withGate bool) (DryReport, error) {
	rep := DryReport{Lock: lockState(r.Cfg.LockFile)}
	rep.Stop = fileExists(r.qpath(fileStop))
	entries, ignored, err := r.listPending()
	if err != nil {
		return rep, err
	}
	rep.Ignored = ignored
	for _, c := range r.listClaims() {
		rep.Claims = append(rep.Claims, c.Path)
	}
	// The heads still owed a run of their own: the state file read without pruning it.
	pendingSpec := map[string]bool{}
	for _, e := range entries {
		pendingSpec[e.Spec()] = true
	}
	for _, spec := range r.soloOwed() {
		if pendingSpec[spec] {
			rep.SoloHeads = append(rep.SoloHeads, spec)
		}
	}
	if base, rc, err := r.gitRO(ctx, r.Cfg.Checkout, "rev-parse", "--verify", "-q", r.Cfg.remoteRef()); err == nil && rc == 0 {
		rep.Base = base
	}
	var planned []Entry
	var firstPR *Entry
	for _, e := range entries {
		d := DryEntry{Entry: e}
		if prev, err := readHead(r.qpath(dirRetried, e.Name)); err == nil && prev == e.Head {
			d.Retried = true
		}
		pass := true
		if d.Landed = r.landedBefore(e); d.Landed != "" {
			pass = false
		}
		if e.HeadOK {
			if _, rc, err := r.gitRO(ctx, r.Cfg.Checkout, "cat-file", "-e", e.Head+"^{commit}"); err == nil && rc == 0 {
				d.InCheckout = true
				if rep.Base != "" {
					if _, rc, err := r.gitRO(ctx, r.Cfg.Checkout, "merge-base", "--is-ancestor", e.Head, rep.Base); err == nil && rc == 0 {
						d.AlreadyInDev = true
					}
				}
			}
		} else {
			pass = false
		}
		// a pull request has no pre-merge gate: the pass reads the pull request instead, which a dry run does not
		if withGate && e.HeadOK && e.PR == 0 {
			note, verdict := r.gate(ctx, e)
			d.Gate = note
			pass = verdict == gatePass
		}
		rep.Entries = append(rep.Entries, d)
		if pass && e.PR > 0 && (firstPR == nil || e.PR < firstPR.PR) {
			pe := e
			firstPR = &pe
		}
		if pass && e.PR == 0 && (len(rep.SoloHeads) == 0 || len(planned) == 0) {
			planned = append(planned, e)
		}
	}
	// a pending pull request is landed alone, before the issue heads
	if firstPR != nil {
		planned = []Entry{*firstPR}
	}
	for _, e := range planned {
		rep.Plan = append(rep.Plan, e.Spec())
	}
	if len(planned) > 1 {
		rep.BatchRetried = fileExists(r.batchMarker(planned))
	}
	return rep, nil
}
