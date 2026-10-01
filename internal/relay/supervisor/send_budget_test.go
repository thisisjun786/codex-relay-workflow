package supervisor

import (
	"math"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-259: a supervisor report spends the hour of the relationship it reports, toward the recipient
// it went to, so the delivery side sees it; another relationship's hour to the same recipient is
// untouched and the recipient's own gap still holds both.
func Test259_a_supervisor_send_is_charged_to_its_relationship_and_recipient(t *testing.T) {
	f, _, id, sent := delivered24(t)
	if sent == nil {
		t.Fatal("the report was not sent")
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	const now = 1_700_000_000
	window := math.Floor(now/3600) * 3600
	if got, err := f.s.RelationshipSends(f.ctx, row.RelationshipID, row.RecipientTaskID, window); err != nil || got != 1 {
		t.Fatalf("the reported relationship's hour count is %d, %v; want its one send", got, err)
	}
	if got, err := f.s.RelationshipSends(f.ctx, "another-relationship", row.RecipientTaskID, window); err != nil || got != 0 {
		t.Fatalf("another relationship's hour count is %d, %v; want 0", got, err)
	}
	service := delivery.NewService(f.s, delivery.SystemClock{})
	service.Policy.MaxSendsPerRelationshipPerHour = 1
	later := float64(now + 60)
	if refusal, err := service.SendRefusal(f.ctx, row.RelationshipID, row.RecipientTaskID, later); err != nil || refusal != delivery.HourlyCap {
		t.Errorf("the reporting relationship, one send into a cap of one: %q, %v; want %s", refusal, err, delivery.HourlyCap)
	}
	if refusal, err := service.SendRefusal(f.ctx, "another-relationship", row.RecipientTaskID, later); err != nil || refusal != "" {
		t.Errorf("another relationship to the same recipient a minute later: %q, %v; want none", refusal, err)
	}
	if refusal, err := service.SendRefusal(f.ctx, "another-relationship", row.RecipientTaskID, now+1); err != nil || refusal != delivery.MinSendInterval {
		t.Errorf("another relationship inside the recipient's gap: %q, %v; want %s", refusal, err, delivery.MinSendInterval)
	}
}
