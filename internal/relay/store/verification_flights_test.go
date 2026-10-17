package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestVerificationFlightAtomicClaimAndCapacity(t *testing.T) {
	s := recordStore(t)
	ctx := context.Background()
	identity := VerificationIdentity{Repository: "o/r", Head: "head", WorkflowID: 77, WorkflowDigest: "source", Rules: "rules", Tools: map[string]string{"go": "1"}}
	var wg sync.WaitGroup
	results := make(chan bool, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, send, err := s.ClaimVerification(ctx, identity, "ci/immutable", 1)
			if err != nil {
				t.Error(err)
			}
			results <- send
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for send := range results {
		if send {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d dispatch grants", count)
	}
	identity.Head = "other"
	if _, _, err := s.ClaimVerification(ctx, identity, "ci/other", 1); err == nil {
		t.Fatal("uncertain dispatch released CI capacity")
	}
}

func TestVerificationDispatchGrantRequiresOwnDurableCommit(t *testing.T) {
	s := recordStore(t)
	err := s.Compose(context.Background(), func(ctx context.Context, _ *sql.Conn) error {
		_, send, err := s.ClaimVerification(ctx, VerificationIdentity{Repository: "o/r", Head: "head", WorkflowID: 77, WorkflowDigest: "source", Rules: "rules", Tools: map[string]string{}}, "ci/ref", 0)
		if err == nil || send {
			t.Error("dispatch granted before its outer transaction committed")
		}
		return errors.New("outer transaction rolled back")
	})
	if err == nil {
		t.Fatal("outer rollback not exercised")
	}
}
