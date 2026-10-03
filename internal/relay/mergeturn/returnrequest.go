package mergeturn

import "context"

// RequestReturn is the red-first stub: it records the request as before and delivers nothing.
func (s *Service) RequestReturn(ctx context.Context, turn, actor, evidence string) (map[string]any, error) {
	return s.Attest(ctx, turn, "return_requested", "return_requested:"+actor, actor, evidence)
}
