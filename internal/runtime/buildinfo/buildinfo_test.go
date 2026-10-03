package buildinfo

import (
	"os"
	"testing"
)

func TestSnapshotUsesStampedBuildOrVersion(t *testing.T) {
	wasVersion, wasBuild := Version, Build
	t.Cleanup(func() { Version, Build = wasVersion, wasBuild })
	Version, Build = "fixture-version", ""
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := Snapshot(); got.Get("build") != "fixture-version" || got.Get("executable") != executable {
		t.Fatal(got)
	}
	Build = "fixture-stamped-build"
	if got := Snapshot(); got.Get("build") != "fixture-stamped-build" || got.Get("executable") != executable {
		t.Fatal(got)
	}
}
