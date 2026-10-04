package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
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
	rebuilt, complete, err := k.sched.rebuildAsConsumed(ctx, k.s.Q(ctx), "rp", snap, node, body)
	if err != nil || !complete || rebuilt != released.ManifestDigest {
		t.Fatalf("revalidation rebuild = %s, %v, %v", rebuilt, complete, err)
	}
	k.seedReport(released.RelationshipID, "A", "rp")
	accepted, err := k.accept("rp", "A", AcceptInput{})
	if err != nil {
		t.Fatal(err)
	}
	criteria := dig("the updated criteria")
	k.rvReregister("rp", "A", "rp-r2", released.RelationshipID, criteria, nil)
	if action := rvAction(t, k.read("rp"), "A"); action != ActionRevalidate {
		t.Fatalf("criteria-only route = %s", action)
	}
	k.rvRule(released.RelationshipID, "verified", criteria, rvVerified())
	again, err := k.accept("rp", "A", AcceptInput{})
	if err != nil || !again.Revalidated || again.AcceptanceID != accepted.AcceptanceID {
		t.Fatalf("revalidation = %+v, %v", again, err)
	}
}

func TestPinnedInputChangedSourceRefusesWithVerifiedRestore(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	source := writeFile(t, k.root, "notes.md", "pinned notes")
	req := k.request(false)
	req.Volatile = []Volatile{{Source: "notes", SnapshotURI: source, SHA256: dig("pinned notes"), CapturedAt: k.clock()}}
	released, err := k.sched.Release(context.Background(), "rp", "A", "parent", req)
	if err != nil {
		t.Fatal(err)
	}
	body, _, _ := k.repo.ReadManifest(context.Background(), released.ManifestDigest)
	pin := body["volatile"].([]any)[0].(map[string]any)["snapshot_uri"].(string)
	writeFile(t, k.root, "notes.md", "changed notes")
	snap := k.snapshot("rp")
	node, _ := nodeOf(snap, "A")
	in := ManifestInput{Volatile: req.Volatile, RuleVersion: req.RuleVersion, CreatedByTaskID: "parent", CreatedAt: k.clock()}
	_, findings, err := k.sched.BuildManifest(context.Background(), k.s.Q(context.Background()), "rp", snap, node, in, VerifyOptions{ArtifactRoots: req.ArtifactRoots})
	if err != nil || len(findings) != 1 || findings[0].Code != "B-04" || !strings.Contains(findings[0].Detail, pin) || !strings.Contains(findings[0].Detail, "restore by copying") {
		t.Fatalf("changed-source refusal = %+v, %v", findings, err)
	}
	if got := refusalReason(refusalOfFinding(findings[0])); got != "manifest_unverified" {
		t.Fatal(got)
	}
	kept, _ := os.ReadFile(pin)
	writeFile(t, k.root, "notes.md", string(kept))
	_, findings, err = k.sched.BuildManifest(context.Background(), k.s.Q(context.Background()), "rp", snap, node, in, VerifyOptions{ArtifactRoots: req.ArtifactRoots})
	if err != nil || len(findings) != 0 {
		t.Fatalf("restored source = %+v, %v", findings, err)
	}
	// An unavailable copy must never be advertised as a recovery source.
	if err := os.Remove(pin); err != nil {
		t.Fatal(err)
	}
	writeFile(t, k.root, "notes.md", "changed again")
	_, findings, _ = k.sched.BuildManifest(context.Background(), k.s.Q(context.Background()), "rp", snap, node, in, VerifyOptions{ArtifactRoots: req.ArtifactRoots})
	if len(findings) != 1 || strings.Contains(findings[0].Detail, "restore by copying") {
		t.Fatalf("offered missing copy = %+v", findings)
	}
}

func TestPinnedInputCopiesNeverFallBackToOriginal(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "linked"} {
		t.Run(kind, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			source := writeFile(t, k.root, "source", "original")
			req := k.request(false)
			req.Volatile = []Volatile{{Source: "doc", SnapshotURI: source, SHA256: dig("original"), CapturedAt: k.clock()}}
			res, err := k.sched.Release(context.Background(), "rp", "A", "parent", req)
			if err != nil {
				t.Fatal(err)
			}
			body, _, _ := k.repo.ReadManifest(context.Background(), res.ManifestDigest)
			pin := body["volatile"].([]any)[0].(map[string]any)["snapshot_uri"].(string)
			if info, err := os.Stat(pin); err != nil || info.Mode().Perm() != 0o400 || info.Size() != int64(len("original")) {
				t.Fatalf("retained mode/size = %v, %v", info, err)
			}
			if err := os.Remove(pin); err != nil {
				t.Fatal(err)
			}
			want := "B-03"
			if kind == "corrupt" {
				writeFile(t, filepath.Dir(pin), filepath.Base(pin), "wrong bytes")
				want = "B-04"
			} else if kind == "linked" {
				if err := os.Symlink(source, pin); err != nil {
					t.Fatal(err)
				}
				want = "B-05"
			}
			snap := k.snapshot("rp")
			node, _ := nodeOf(snap, "A")
			findings, err := k.sched.VerifyManifest(context.Background(), k.s.Q(context.Background()), "rp", snap, node, body, VerifyOptions{ArtifactRoots: req.ArtifactRoots})
			if err != nil || len(findings) != 1 || findings[0].Code != want {
				t.Fatalf("retained-copy verification = %+v, %v", findings, err)
			}
		})
	}
}

func TestPinnedInputScopeIsExactEvenWithoutFileReads(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	source := writeFile(t, k.root, "source", "original")
	req := k.request(false)
	req.Volatile = []Volatile{{Source: "doc", SnapshotURI: source, SHA256: dig("original"), CapturedAt: k.clock()}}
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", req)
	if err != nil {
		t.Fatal(err)
	}
	snap := k.snapshot("rp")
	node, _ := nodeOf(snap, "A")
	root := filepath.Join(filepath.Dir(k.s.Path), "dag-input-snapshots", dig("rp"))
	for _, path := range []string{root + "/sibling", root + "/../" + dig("other-plan") + "/" + dig("original"), root + "/../" + dig("rp") + "/" + dig("original")} {
		body, _, _ := k.repo.ReadManifest(context.Background(), res.ManifestDigest)
		body["volatile"].([]any)[0].(map[string]any)["snapshot_uri"] = path
		body["manifest_digest"] = dag.ManifestDigest(body)
		findings, err := k.sched.VerifyManifest(context.Background(), k.s.Q(context.Background()), "rp", snap, node, body, VerifyOptions{ArtifactRoots: req.ArtifactRoots, SkipFileBytes: true})
		if err != nil || len(findings) != 1 || findings[0].Code != "B-05" {
			t.Fatalf("scope for %q = %+v, %v", path, findings, err)
		}
	}
}

func TestPinnedInputCorrectionUsesCopiesAndKeepsLegacyInstructions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new retained input", true: "legacy prepared input"}[legacy], func(t *testing.T) {
			k := newReleaseKit(t)
			rid := k.correctionKit() // The initial release has no volatile inputs.
			source := writeFile(t, k.root, "correction.md", "correction input")
			in := ManifestInput{RuleVersion: k.request(false).RuleVersion, Volatile: []Volatile{{Source: "notes", SnapshotURI: source, SHA256: dig("correction input"), CapturedAt: k.clock()}}, CreatedByTaskID: "parent", CreatedAt: k.clock()}
			ctx := context.Background()
			var prepared Prepared
			if legacy {
				snap := k.snapshot("rp")
				node, _ := nodeOf(snap, "A")
				body, findings, err := k.sched.BuildManifest(ctx, k.s.Q(ctx), "rp", snap, node, in, VerifyOptions{ArtifactRoots: []string{k.root}})
				if err != nil || len(findings) != 0 {
					t.Fatalf("legacy manifest = %+v, %v", findings, err)
				}
				raw, _ := json.Marshal(body)
				digest, err := k.repo.PutManifest(ctx, raw)
				if err != nil {
					t.Fatal(err)
				}
				canonical := []byte(dag.Canonical(body))
				path, err := FreezeManifest(k.root, canonical)
				if err != nil {
					t.Fatal(err)
				}
				prepared = Prepared{ManifestDigest: digest, FrozenPath: path, Instruction: CorrectionInstruction("CRW-A", 2, digest, path, shaOf(canonical))}
			} else {
				var err error
				prepared, err = k.sched.PrepareCorrection(ctx, "rp", "A", "parent", in, VerifyOptions{ArtifactRoots: []string{k.root}})
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, k.root, "correction.md", "overwritten correction input")
				if !strings.Contains(prepared.Instruction, "authorized as read-only inputs") {
					t.Fatal("correction omitted retained-input read authorization")
				}
				body, _, _ := k.repo.ReadManifest(ctx, prepared.ManifestDigest)
				entry := body["volatile"].([]any)[0].(map[string]any)
				pin := entry["snapshot_uri"].(string)
				kept, err := os.ReadFile(pin)
				if err != nil || string(kept) != "correction input" {
					t.Fatalf("correction input = %q, %v", kept, err)
				}
				// Explicitly preparing from the kept path works after the original changes.
				in.Volatile[0].SnapshotURI = pin
				again, err := k.sched.PrepareCorrection(ctx, "rp", "A", "parent", in, VerifyOptions{ArtifactRoots: []string{k.root}})
				if err != nil || again.Instruction != prepared.Instruction {
					t.Fatalf("prepare from kept input = %+v, %v", again, err)
				}
			}
			k.openCorrection(rid, []map[string]any{{"id": "c1", "restoration": true, "note": prepared.Instruction}})
			if !legacy {
				withoutScope, _ := json.Marshal([]map[string]any{{"id": "c1", "restoration": true, "note": strings.TrimSuffix(prepared.Instruction, pinnedInputReadInstruction)}})
				k.exec("UPDATE verdict_context SET findings = ?", string(withoutScope))
				if _, err := k.correct(prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" {
					t.Fatalf("new pinned input without read authorization = %v", err)
				}
				withScope, _ := json.Marshal([]map[string]any{{"id": "c1", "restoration": true, "note": prepared.Instruction}})
				k.exec("UPDATE verdict_context SET findings = ?", string(withScope))
			}
			if got, err := k.correct(prepared.ManifestDigest); err != nil || got.ManifestDigest != prepared.ManifestDigest {
				t.Fatalf("record correction = %+v, %v", got, err)
			}
		})
	}
}

func TestPinnedInputPublicationIsSharedAndConfined(t *testing.T) {
	for _, linked := range []string{"", "snapshot directory", "plan directory"} {
		t.Run(linked, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			source := writeFile(t, k.root, "source", "shared input")
			req := k.request(false)
			req.Volatile = []Volatile{{Source: "doc", SnapshotURI: source, SHA256: dig("shared input"), CapturedAt: k.clock()}}
			state := filepath.Dir(k.s.Path)
			outside := t.TempDir()
			if linked != "" {
				path := filepath.Join(state, "dag-input-snapshots")
				if linked == "plan directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(path, dig("rp"))
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
				if _, err := k.sched.Release(context.Background(), "rp", "A", "parent", req); err == nil {
					t.Fatal("release followed a linked input-copy directory")
				}
				if files, err := os.ReadDir(outside); err != nil || len(files) != 0 {
					t.Fatalf("wrote outside state: %v, %v", files, err)
				}
				if k.rows().releases != 0 || k.host.created != 0 {
					t.Fatal("failed pin released a node")
				}
				return
			}
			var wg sync.WaitGroup
			errs := make([]error, 4)
			for i := range errs {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, errs[i] = k.sched.Release(context.Background(), "rp", "A", "parent", req)
				}(i)
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			files, err := os.ReadDir(filepath.Join(state, "dag-input-snapshots", dig("rp")))
			if err != nil || len(files) != 1 || files[0].Name() != dig("shared input") {
				t.Fatalf("copies/temp residues = %v, %v", files, err)
			}
			if created, _ := k.host.counts(); created != 1 {
				t.Fatalf("created %d children", created)
			}
		})
	}
}

func TestPinnedInputUnreleasedSourcesStayEditable(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	source := writeFile(t, k.root, "source", "first version")
	writeFile(t, k.root, "source", "updated before release")
	req := k.request(false)
	req.Volatile = []Volatile{{Source: "doc", SnapshotURI: source, SHA256: dig("updated before release"), CapturedAt: k.clock()}}
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", req)
	if err != nil || !res.Bound {
		t.Fatalf("updated input release = %+v, %v", res, err)
	}
	info, err := os.Stat(source)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("source permissions changed = %v, %v", info, err)
	}
}

func TestPinnedInputFailedCopyPublishesNothing(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong bytes", true: "canceled read"}[canceled], func(t *testing.T) {
			k := newReleaseKit(t)
			source := writeFile(t, k.root, "source", "changed before copying")
			root := k.sched.pinnedInputRoot("rp")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			err := pinnedInputPublish(ctx, source, dig("original"), root, []string{k.root})
			if err == nil || (canceled && !errors.Is(err, context.Canceled)) {
				t.Fatalf("failed copy = %v", err)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 {
				t.Fatalf("partial copy or temp survived = %v, %v", files, err)
			}
		})
	}
}

func TestPinnedInputRelativeStorePathUsesSelectedState(t *testing.T) {
	k := newReleaseKit(t)
	t.Chdir(filepath.Dir(k.s.Path))
	k.s.Path = filepath.Base(k.s.Path)
	releasePlan(k.fixture, "rp")
	source := writeFile(t, k.root, "source", "relative store input")
	req := k.request(false)
	req.Volatile = []Volatile{{Source: "doc", SnapshotURI: source, SHA256: dig("relative store input"), CapturedAt: k.clock()}}
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", req)
	if err != nil || !res.Bound {
		t.Fatalf("release with relative store path = %+v, %v", res, err)
	}
	body, _, _ := k.repo.ReadManifest(context.Background(), res.ManifestDigest)
	path := body["volatile"].([]any)[0].(map[string]any)["snapshot_uri"].(string)
	if !filepath.IsAbs(path) || !strings.HasPrefix(path, filepath.Dir(k.path)+"/") {
		t.Fatalf("retained outside the selected state: %q", path)
	}
}
