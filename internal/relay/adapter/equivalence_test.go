package adapter

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func Test28_MSC_10_TwoValueAndDetailedVerifiers(t *testing.T) {
	for _, kind := range []string{"good", "deleted", "tampered", "absent", "different"} {
		t.Run(kind, func(t *testing.T) {
			file, reference, entries := frozenFixture(t)
			switch kind {
			case "deleted":
				if err := os.Remove(filepath.Join(reference, "files", entries[0].SHA256)); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				if err := os.WriteFile(filepath.Join(reference, "files", entries[0].SHA256), []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "absent":
				reference = filepath.Join(filepath.Dir(file), "equiv-absent")
			case "different":
				entries[0].SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			digest, problems, err := VerifyFrozen(reference, entries)
			if err != nil {
				t.Fatal(err)
			}
			detailed, all, _, err := VerifyFrozenDetailed(reference, entries)
			if err != nil {
				t.Fatal(err)
			}
			if digest != detailed || !reflect.DeepEqual(problems, all) {
				t.Fatalf("two-value frozen differs: %s %v vs %s %v", digest, problems, detailed, all)
			}
			frozenCapture(t, reference, entries)
		})
	}
	for _, kind := range []string{"good", "gone", "changed", "outside", "empty"} {
		t.Run("live-"+kind, func(t *testing.T) {
			file, _, entries := frozenFixture(t)
			roots := []string{filepath.Dir(file)}
			switch kind {
			case "gone":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "changed":
				if err := os.WriteFile(file, []byte("a later revision"), 0600); err != nil {
					t.Fatal(err)
				}
			case "outside":
				roots = []string{filepath.Join(filepath.Dir(file), "elsewhere")}
			case "empty":
				entries = []Entry{}
			}
			problems, bindings := VerifyAgainstDisk(entries, roots, false)
			all, details, _ := VerifyAgainstDiskDetailed(entries, roots, false)
			if !reflect.DeepEqual(problems, all) || !reflect.DeepEqual(bindings, details) {
				t.Fatal("two-value live differs")
			}
			verifyCapture(t, entries, roots)
		})
	}
}
