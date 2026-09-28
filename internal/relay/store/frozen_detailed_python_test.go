package store

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func Test28FrozenDetailedJSONParity(t *testing.T) {
	repo, _ := filepath.Abs("../../..")
	for _, tc := range []struct{ name, manifest string }{{"float-bytes", `{"entries":[{"path":"/a","sha256":"0000000000000000000000000000000000000000000000000000000000000000","bytes":1.0}]}`}, {"missing-digest", `{"entries":[{"path":"/a"}]}`}} {
		t.Run(tc.name, func(t *testing.T) {
			ref := t.TempDir()
			if err := os.WriteFile(filepath.Join(ref, "MANIFEST.json"), []byte(tc.manifest), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/store/testdata/frozen_detailed.py"), ref)
			cmd.Dir = repo
			want, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(want))
			}
			result, problems, unreadable, goErr := VerifyFrozenDetailed(ref, nil)
			got := map[string]any{}
			if goErr != nil {
				got["error"] = "KeyError"
				got["detail"] = goErr.Error()
			} else {
				got["result"] = []any{result, problems, unreadable}
			}
			raw, _ := json.Marshal(got)
			var w, g any
			_ = json.Unmarshal(want, &w)
			_ = json.Unmarshal(raw, &g)
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("Go %s Python %s", raw, want)
			}
		})
	}
}

func Test28FrozenDetailedManifestStatParity(t *testing.T) {
	ref := t.TempDir()
	if err := os.Symlink("MANIFEST.json", filepath.Join(ref, "MANIFEST.json")); err != nil {
		t.Fatal(err)
	}
	_, problems, unreadable, err := VerifyFrozenDetailed(ref, nil)
	if err != nil || len(problems) != 1 || len(unreadable) != 0 {
		t.Fatalf("%v %v %v", problems, unreadable, err)
	}
	if !bytes.Contains([]byte(problems[0]), []byte("no MANIFEST.json")) {
		t.Fatal(problems)
	}
}
