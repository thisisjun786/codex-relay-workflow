package store

import (
	"context"
	"time"
)

// fixtureOpen is the writable opener of the store tests, with the busy timeout they were written
// for. It provisions nothing: Open itself creates an absent store as owner=go at epoch 1, the
// state a fresh Go host has. A test that needs a store another runtime wrote, a fixture, or a
// copy puts it into Go's ownership explicitly with testsupport.HandOver, Fence or Rehome, and a
// fence refusal test calls Open directly.
func fixtureOpen(ctx context.Context, path, socket string) (*Store, error) {
	return fixtureOpenWith(ctx, path, socket, OpenOptions{BusyTimeout: 30 * time.Second})
}
func fixtureOpenWith(ctx context.Context, path, socket string, o OpenOptions) (*Store, error) {
	return OpenWith(ctx, path, socket, o)
}
