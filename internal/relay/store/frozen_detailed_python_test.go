package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test28FrozenDetailedJSONParity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, manifest string }{{"float-bytes", `{"entries":[{"path":"/a","sha256":"0000000000000000000000000000000000000000000000000000000000000000","bytes":1.0}]}`}, {"missing-digest", `{"entries":[{"path":"/a"}]}`}} {
		t.Run(tc.name, func(t *testing.T) {
			ref := t.TempDir()
			if err := os.WriteFile(filepath.Join(ref, "MANIFEST.json"), []byte(tc.manifest), 0600); err != nil {
				t.Fatal(err)
			}
			result, problems, unreadable, goErr := VerifyFrozenDetailed(context.Background(), ref, nil)
			got := map[string]any{}
			if goErr != nil {
				got["error"] = "KeyError"
				got["detail"] = goErr.Error()
			} else {
				got["result"] = []any{result, problems, unreadable}
			}
			raw, _ := json.Marshal(got)
			var goValue any
			if err := json.Unmarshal(raw, &goValue); err != nil {
				t.Fatal(err)
			}
			checkJSON(t, "answer", goValue, golden.Substitute(ref, "<REF>"))
		})
	}
}

func Test28FrozenDetailedManifestStatParity(t *testing.T) {
	t.Parallel()
	ref := t.TempDir()
	if err := os.Symlink("MANIFEST.json", filepath.Join(ref, "MANIFEST.json")); err != nil {
		t.Fatal(err)
	}
	_, problems, unreadable, err := VerifyFrozenDetailed(context.Background(), ref, nil)
	if err != nil || len(problems) != 1 || len(unreadable) != 0 {
		t.Fatalf("%v %v %v", problems, unreadable, err)
	}
	if !bytes.Contains([]byte(problems[0]), []byte("no MANIFEST.json")) {
		t.Fatal(problems)
	}
}
