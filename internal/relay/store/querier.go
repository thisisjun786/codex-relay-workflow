package store

import "context"

// Querier is the ctx-aware querier: the open transaction's connection when ctx carries one of
// this store's transactions, otherwise the pool. Domain packages that run their own SQL (the
// registry) read and write through it so a read inside a transaction sees that transaction's
// own writes, as Python's single connection does.
type Querier = querier

// Querier returns where a statement issued under ctx runs (see q).
func (s *Store) Querier(ctx context.Context) Querier { return s.q(ctx) }

// SetFaultHook installs the hook every opened transaction runs after its body and before
// COMMIT (store.py fault_hook). Tests only; nil removes it.
func (s *Store) SetFaultHook(hook func()) { s.faultHook = hook }

// InTransaction is Python's store.in_transaction for the call chain ctx belongs to.
func (s *Store) InTransaction(ctx context.Context) bool {
	open, ok := ctx.Value(openTxKey{}).(openTx)
	return ok && open.store == s
}
