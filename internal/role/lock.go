package role

import "time"

// lockStore is the stub of the red step: it takes no lock.
func lockStore(path string, sleep func(time.Duration)) (func(), error) { return func() {}, nil }
