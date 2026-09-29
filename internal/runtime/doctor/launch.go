package doctor

// LaunchProblems is what keeps a host from launching the Go runtime at target, as the doctor
// judges it for a selected install: bin/crw must be a regular file this user may execute, and
// each compatibility link beside it must resolve to it. unread is what could not be examined,
// which is never read as launchable. crw install rollback asks it of a Go runtime before it
// points the pointer there.
func LaunchProblems(target string) (problems, unread []string) { return launchProblems(target) }
