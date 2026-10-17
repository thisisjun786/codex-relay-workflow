package cli

// FollowDone reports whether a follower has drained a run that no process can
// currently append to. Live is the commit barrier for every stopped state: a
// writer may still be about to commit rows even when the run process has ended.
func (p Page) FollowDone() bool {
	return len(p.Rows) == 0 && !p.More && !p.Live &&
		(Over(p.State) || p.State == StateDead || p.State == StateHeld || p.State == StatePaused)
}
