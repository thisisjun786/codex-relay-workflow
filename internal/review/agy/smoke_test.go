//go:build agysmoke

package agy

import (
	"context"
	"testing"
	"time"
)

// TestSmokeRealAgy calls the real agy once, with the default model, the host-wide lock and the scrubbed environment, and prints what came back. It is built only with
// the agysmoke tag (go test -tags agysmoke -run TestSmokeRealAgy -v ./internal/review/agy) and is meant to be run by hand: it uses the account agy is logged in with.
// The prompt holds a non-ASCII character and an emoji, so a promptLength that counts anything but bytes fails as a prompt length mismatch with both numbers in the detail.
func TestSmokeRealAgy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prompt := []byte("Answer with the single word PONG in the answer field. Marker: é 😀.")
	schema := []byte(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	res, err := Run(ctx, Config{WorkRoot: t.TempDir()}, Request{Prompt: prompt, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("class=%s reason=%q detail=%q model=%q exit=%d elapsed=%s waited=%s limit=%s usage=%+v prompt=%d bytes structured=%s",
		res.Class, res.Reason, res.Detail, res.Model, res.ExitCode, res.Elapsed, res.Waited, res.Limit, res.Usage, len(prompt), res.StructuredOutput)
	if res.Class != ClassNormal {
		t.Fatalf("the call was %s: %s", res.Class, res.Reason)
	}
}
