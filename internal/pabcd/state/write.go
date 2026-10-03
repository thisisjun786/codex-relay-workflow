package state

import (
	"errors"
	"time"
)

// Skeleton for the red run: the API the tests call, with no behaviour yet.

// ErrNonCanonicalSessionID is the TypeError ensureState throws for an id that sanitising would rewrite.
const ErrNonCanonicalSessionID = sentinel("sessionId must be a canonical state key")

type sentinel string

func (e sentinel) Error() string { return string(e) }

func unimplemented() error { return errors.New("not implemented") }

// EnsureState creates the session's file as a fresh IDLE state and reports whether it did.
func EnsureState(cwd, sessionID string) (bool, error) { return false, unimplemented() }

func ensureState(cwd, sessionID string, now time.Time, link func(existing, created string) error) (bool, error) {
	return false, unimplemented()
}

// WriteState publishes next as the session's state file.
func WriteState(cwd string, next State) error { return unimplemented() }

func writeState(cwd string, next State, now time.Time, rename func(tmp, finalPath string) error) error {
	return unimplemented()
}

func makeSessionsDir(cwd string) error { return unimplemented() }
