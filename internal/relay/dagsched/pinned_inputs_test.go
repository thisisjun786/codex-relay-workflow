package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedInputReleaseSurvivesSourceOverwrite(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	original := "the input bytes the released child must consume"
	source := writeFile(t, k.root, "issue.md", original)
	req := k.request(false)
	req.Volatile = []Volatile{{Source: "issue:test", SnapshotURI: source, SHA256: dig(original), CapturedAt: k.clock()}}
	ctx := context.Background()
	released, err := k.sched.Release(ctx, "rp", "A", "parent", req)
	if err != nil || !released.Bound {
		t.Fatalf("release = %+v, %v", released, err)
	}
	writeFile(t, k.root, "issue.md", "the parent overwrote the input")
	body, found, err := k.repo.ReadManifest(ctx, released.ManifestDigest)
	if err != nil || !found {
		t.Fatalf("manifest = %v, %v", found, err)
	}
	snap := k.snapshot("rp")
	node, _ := nodeOf(snap, "A")
	findings, err := k.sched.VerifyManifest(ctx, k.s.Q(ctx), "rp", snap, node, body, VerifyOptions{ArtifactRoots: req.ArtifactRoots})
	if err != nil || len(findings) != 0 {
		t.Fatalf("released snapshot verification stopped after source overwrite: %+v, %v", findings, err)
	}
	entry := body["volatile"].([]any)[0].(map[string]any)
	pin := entry["snapshot_uri"].(string)
	want := filepath.Join(filepath.Dir(k.s.Path), "dag-input-snapshots", dig("rp"), dig(original))
	if pin != want || !strings.Contains(k.host.sent[0], pin) {
		t.Fatalf("child received %q, want retained path %q", pin, want)
	}
	bytes, err := os.ReadFile(pin)
	if err != nil || string(bytes) != original {
		t.Fatalf("child's pinned input = %q, %v", bytes, err)
	}
}
