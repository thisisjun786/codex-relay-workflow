package residue_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/residue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	golden.Helper()
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func staged(t *testing.T, dir, state string) {
	t.Helper()
	if err := staging.WriteClaim(dir, staging.Payload(state, staging.WrittenByPython, nil, nil, 1, "h", "t")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "half-built"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func nothingSelected(string) (bool, *bool) {
	no := false
	return false, &no
}

func entryFor(survey record.Object, path string) record.Object {
	for _, raw := range golden.List(record.Get(survey, "entries")) {
		if e := golden.Obj(raw); record.Get(e, "path") == path {
			return e
		}
	}
	return nil
}

func residual(survey record.Object) []string {
	var out []string
	for _, p := range golden.List(record.Get(survey, "residualPaths")) {
		out = append(out, p.(string))
	}
	return out
}

// Residue is exactly what the installer's decision would reclaim: an abandoned staging this
// command claimed. A live staging, a finished environment, a selected one, somebody else's
// directory and a symbolic link are never named.
func TestResidueIsWhatTheInstallerWouldReclaim(t *testing.T) {
	dest := t.TempDir()
	abandoned := filepath.Join(dest, ".bin-0.3.0-aaaaaaaaaaaa.tmp-1")
	live := filepath.Join(dest, ".bin-0.3.0-bbbbbbbbbbbb.tmp-2")
	finished := filepath.Join(dest, "env-1-cccccccccccc")
	foreign := filepath.Join(dest, "somebody")
	staged(t, abandoned, staging.Staging)
	staged(t, live, staging.Staging)
	staged(t, finished, staging.Complete)
	if err := os.MkdirAll(filepath.Join(foreign, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(abandoned, filepath.Join(dest, "alias")); err != nil {
		t.Fatal(err)
	}
	held, err := staging.Take(live)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	survey := residue.Survey(&dest, "", nil, nothingSelected, nil)
	if got := residual(survey); len(got) != 1 || got[0] != abandoned {
		t.Fatalf("residual %v\n%s", got, golden.Canon(survey))
	}
	for path, decision := range map[string]string{live: staging.Occupied, finished: staging.Keep, foreign: staging.Foreign, filepath.Join(dest, "alias"): residue.NotScanned} {
		if got := record.Get(entryFor(survey, path), "decision"); got != decision {
			t.Errorf("%s: %v, want %s", path, got, decision)
		}
	}
	selected := func(dir string) (bool, *bool) {
		yes := dir == abandoned
		return yes, &yes
	}
	if got := residual(residue.Survey(&dest, "", nil, selected, nil)); len(got) != 0 {
		t.Fatalf("a selected staging was named as residue: %v", got)
	}
	if survey := residue.Survey(&dest, "", nil, nil, nil); len(residual(survey)) != 0 || record.Get(survey, "ownershipRead") != false {
		t.Fatalf("no ownership reading, yet residue: %s", golden.Canon(survey))
	}
}

// A dangling pointer is residue only when the record positively records THIS path as a link
// this command placed; a rollback that withdrew the placement, a pointer another installation
// owns, and a target that could not be read are all reported and never listed.
func TestAPointerIsResidueOnlyWithPlacementEvidence(t *testing.T) {
	dest := t.TempDir()
	path := pointer.Path(dest)
	if err := pointer.Place(path, filepath.Join(dest, "gone")); err != nil {
		t.Fatal(err)
	}
	placed := record.Object{{Key: "path", Value: path}, {Key: "recordedAt", Value: "t"}, {Key: "recordedBy", Value: "CRW-157"}}
	survey := residue.Survey(&dest, path, placed, nothingSelected, nil)
	finding := golden.Obj(record.Get(survey, "pointer"))
	if record.Get(finding, "finding") != residue.DanglingPointer || len(residual(survey)) != 1 || residual(survey)[0] != path {
		t.Fatalf("an owned dangling pointer: %s", golden.Canon(survey))
	}
	if record.Get(entryFor(survey, path), "decision") != residue.NotScanned {
		t.Fatal("the pointer was scanned as an environment")
	}
	withdrawn := record.WithoutPlacement(placed)
	survey = residue.Survey(&dest, path, withdrawn, nothingSelected, nil)
	if finding := golden.Obj(record.Get(survey, "pointer")); record.Get(finding, "finding") != residue.ForeignPointer || len(residual(survey)) != 0 {
		t.Fatalf("a withdrawn placement: %s", golden.Canon(finding))
	}
	elsewhere := t.TempDir()
	survey = residue.Survey(&elsewhere, path, placed, nothingSelected, nil)
	if finding := golden.Obj(record.Get(survey, "pointer")); record.Get(finding, "finding") != residue.PointerOutsideDestination || len(residual(survey)) != 0 {
		t.Fatalf("another installation's pointer: %s", golden.Canon(finding))
	}
	if err := pointer.Place(path, filepath.Join(dest, "real")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if survey := residue.Survey(&dest, path, placed, nothingSelected, nil); len(residual(survey)) != 0 {
		t.Fatalf("a pointer that reaches its target is not residue: %s", golden.Canon(survey))
	}
}

// A destination that cannot be listed, or none named, is an unreadable reading and never an
// empty cleanup list; the pointer is still read on that path.
func TestAnUnlistedDestinationIsReportedNotEmpty(t *testing.T) {
	survey := residue.Survey(nil, "", nil, nothingSelected, nil)
	if golden.Canon(record.Get(survey, "unreadable")) != `["no destination was named, so nothing was scanned"]` || record.Get(survey, "read") != false {
		t.Fatalf("no destination: %s", golden.Canon(survey))
	}
	missing := filepath.Join(t.TempDir(), "missing")
	survey = residue.Survey(&missing, pointer.Path(missing), nil, nothingSelected, []string{"the caller's own failure"})
	unread := golden.List(record.Get(survey, "unreadable"))
	if len(unread) != 2 || unread[0] != "the caller's own failure" || record.Get(survey, "pointer") == nil {
		t.Fatalf("an unlistable destination: %s", golden.Canon(survey))
	}
}

// Destinations are compared by what the kernel says, not by spelling: an alias of the
// destination is the destination (its pointer is excluded through the alias), a child that
// merely shares a pointer's name elsewhere is still scanned, and a pointer target that could
// not be read is reported rather than listed.
func TestDestinationsAreComparedByTheFilesystem(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "runtime")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(dest, alias); err != nil {
		t.Fatal(err)
	}
	path := pointer.Path(dest)
	if err := pointer.Place(path, filepath.Join(dest, "gone")); err != nil {
		t.Fatal(err)
	}
	placed := record.Object{{Key: "path", Value: path}, {Key: "recordedAt", Value: "t"}, {Key: "recordedBy", Value: "CRW-157"}}
	survey := residue.Survey(&alias, path, placed, nothingSelected, nil)
	if record.Get(entryFor(survey, filepath.Join(alias, "current")), "decision") != residue.NotScanned || len(residual(survey)) != 1 {
		t.Fatalf("through an alias: %s", golden.Canon(survey))
	}

	other := t.TempDir()
	child := filepath.Join(other, "current")
	staged(t, child, staging.Staging)
	survey = residue.Survey(&other, path, placed, nothingSelected, nil)
	if record.Get(entryFor(survey, child), "decision") != staging.Reclaim {
		t.Fatalf("a child sharing a pointer's name elsewhere was not scanned: %s", golden.Canon(survey))
	}

	if os.Geteuid() != 0 {
		locked := filepath.Join(dest, "locked")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := pointer.Place(path, filepath.Join(locked, "target")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(locked, 0o755)
		survey = residue.Survey(&dest, path, placed, nothingSelected, nil)
		if finding := golden.Obj(record.Get(survey, "pointer")); record.Get(finding, "finding") != residue.UnreadablePointerTarget || record.Get(finding, "residual") != false {
			t.Fatalf("an unreadable target: %s", golden.Canon(finding))
		}
	}
}
