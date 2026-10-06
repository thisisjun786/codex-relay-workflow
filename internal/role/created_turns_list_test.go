package role

import (
	"context"
	"errors"
	"testing"
)

// A turn list that cannot be read closes with the note, as CRW-629 had it, whatever the thread/read answer carried.
func TestCreatedTurnsListErrorClosesWithTheNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    *createdRuntimeHost
	}{
		{"no turns field", &createdRuntimeHost{turnsField: "absent", turnErr: errors.New("host unavailable")}},
		{"turns null", &createdRuntimeHost{turnsField: "null", turnErr: errors.New("host unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, attempt, _ := createdRuntimeStart(t, false)
			out, err := CheckedDispatch(context.Background(), ws, createdRuntimeStop(attempt), env, tc.h)
			check(t, err)
			if out.Action != "stop" || out.Reason != createdRuntimeClosed+"; "+createdRuntimeNote {
				t.Fatalf("close = %q %q", out.Action, out.Reason)
			}
		})
	}
}
