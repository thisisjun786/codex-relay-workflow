//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCRW1590VerifierIdentityRejects(t *testing.T) {
	for _, field := range []string{"kind", "rules"} {
		t.Run(field, func(t *testing.T) {
			repo, base, node := crw1590Fixture(t)
			mark, records := t.TempDir(), t.TempDir()
			path := filepath.Join(records, "source.json")
			opts := crw1590Options(repo, mark, base, node, path)
			source, _ := crw1590Verify(t, opts, "")
			if field == "kind" {
				source.VerifierKind = "github-actions"
			} else {
				source.VerifierRules = "different"
			}
			crw1590Write(t, path, source)
			opts.Record = filepath.Join(records, "new.json")
			got, reused := crw1590Verify(t, opts, path)
			if reused || got.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
				t.Errorf("different verifier %s reused=%t result=%s", field, reused, got.Result)
			}
			crw1590NoReuse(t, got)
		})
	}
}

func TestCRW1590CacheAcrossWorktreesAndFresh(t *testing.T) {
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	opts := crw1590Options(repo, mark, base, node, filepath.Join(records, "node.json"))
	opts.Cache, opts.Runner = true, "flow"
	source, reused := crw1590Verify(t, opts, "")
	if reused || source.Result != localPass {
		t.Fatalf("source reused=%t result=%s", reused, source.Result)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.git("worktree", "add", "--detach", linked, node)
	t.Cleanup(func() { repo.git("worktree", "remove", "--force", linked) })
	opts.Root, opts.Runner, opts.Record = linked, "local", filepath.Join(records, "close.json")
	got, reused := crw1590Verify(t, opts, "")
	if !reused || got.Result != localPass || got.Runner != "local" ||
		localMarkerLines(t, filepath.Join(mark, "tree")) != 1 || localMarkerLines(t, filepath.Join(mark, "range")) != 2 {
		t.Fatalf("linked-worktree reuse=%t record=%+v", reused, got)
	}
	ref := crw1590Source(t, got.Jobs[1].Steps[0])
	path, ok := ref["record"].(string)
	if !ok {
		t.Fatalf("missing durable source: %v", ref)
	}
	cached, err := readRecord(path)
	if err != nil || cached.Digest != ref["digest"] || cached.HeadCommit != node {
		t.Fatalf("source cache record=%+v error=%v", cached, err)
	}
	opts.Fresh, opts.Record = true, filepath.Join(records, "fresh.json")
	fresh, reused := crw1590Verify(t, opts, "")
	if reused || fresh.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
		t.Fatalf("fresh reused=%t result=%s", reused, fresh.Result)
	}
	crw1590NoReuse(t, fresh)
	dir, err := localCacheDirectory(opts, fresh)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := recordDigest(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")+".json")); err != nil {
		t.Errorf("fresh PASS was not published: %v", err)
	}
	if _, _, err := localVerify(opts, path, io.Discard); err == nil {
		t.Error("fresh plus explicit reuse must be refused")
	}
}

func TestCRW1590CacheDoesNotPublishFailure(t *testing.T) {
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	opts := crw1590Options(repo, mark, base, node, filepath.Join(records, "failed.json"))
	opts.Cache = true
	opts.Env = append(opts.Env, "CRW1590_FAIL_TREE=1")
	failed, _ := crw1590Verify(t, opts, "")
	if failed.Result != localFail {
		t.Fatal("fixture must actually fail")
	}
	dir, err := localCacheDirectory(opts, failed)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed run published cache entries: %v", entries)
	}
	opts.Env = []string{"CRW1590_MARK=" + mark, "CRW1590_FAIL_HEAD="}
	opts.Record = filepath.Join(records, "good.json")
	got, reused := crw1590Verify(t, opts, "")
	if reused || got.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
		t.Errorf("failed-cache candidate reused=%t result=%s", reused, got.Result)
	}
}

func TestCRW1590CacheIdempotentAndCorruptionMisses(t *testing.T) {
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	opts := crw1590Options(repo, mark, base, node, filepath.Join(records, "source.json"))
	source, _ := crw1590Verify(t, opts, "")
	if source.Result != localPass || source.Digest != "" {
		t.Fatal("fixture must return an unsealed PASS for the publication test")
	}
	first := localCachePass(opts, source, opts.Plan, io.Discard)
	second := localCachePass(opts, source, opts.Plan, io.Discard)
	if first == "" || first != second {
		t.Fatalf("idempotent publication: %q then %q", first, second)
	}
	if _, err := readRecord(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.Cache, opts.Record = true, filepath.Join(records, "new.json")
	got, reused := crw1590Verify(t, opts, "")
	if reused || got.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
		t.Errorf("corrupt cache reused=%t result=%s", reused, got.Result)
	}
}

func TestCRW1590AllTreeMatrixProvenance(t *testing.T) {
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	plan := []localJob{
		{name: "matrix", legs: []string{"one", "two"}, steps: []localStep{
			{name: "common", kind: localRun, scope: "full", command: `echo common >> "$CRW1590_MARK/common"`},
			{name: "only one", kind: localRun, scope: "full", legs: []string{"one"}, command: `echo one >> "$CRW1590_MARK/one"`},
		}},
		{name: "after", steps: []localStep{{name: "last", kind: localRun, scope: "full", command: `echo last >> "$CRW1590_MARK/last"`}}},
	}
	path := filepath.Join(records, "source.json")
	opts := crw1590Options(repo, mark, base, node, path)
	opts.Plan = plan
	source, _ := crw1590Verify(t, opts, "")
	crw1590Write(t, path, source)
	opts.Record = filepath.Join(records, "reused.json")
	got, reused := crw1590Verify(t, opts, path)
	if !reused || got.Result != localPass || got.Digest == "" {
		t.Fatalf("matrix reused=%t result=%s digest=%s", reused, got.Result, got.Digest)
	}
	for i, expected := range localExpectedJobs(plan) {
		for n := range expected.steps {
			ref := crw1590Source(t, got.Jobs[i].Steps[n])
			if ref == nil || ref["job"] != expected.name || ref["step"] != float64(n) {
				t.Errorf("%s step%d provenance=%v", expected.name, n, ref)
			}
		}
	}
	for name, want := range map[string]int{"common": 2, "one": 1, "last": 1} {
		if got := localMarkerLines(t, filepath.Join(mark, name)); got != want {
			t.Errorf("%s executed %d times, want %d", name, got, want)
		}
	}
	if ok, reason := localValidateReuse(got, plan); !ok {
		t.Errorf("matrix output no longer matches its plan: %s", reason)
	}
}
