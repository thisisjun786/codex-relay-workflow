package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func Test28FrozenByteCountExactPythonParity(t *testing.T) {
	cases := []struct {
		name, token string
		present     bool
	}{
		{"fraction", "1.5", true},
		{"float-integral", "1.0", true},
		{"exponent-integral", "1e0", true},
		{"large-exponent", "1e20", true},
		{"above-2pow53", "9007199254740993", true},
		{"above-int64", "9223372036854775808", true},
		{"negative", "-1", true},
		{"true", "true", true},
		{"false", "false", true},
		{"null", "null", true},
		{"missing", "", false},
	}
	repo, _ := filepath.Abs("../../..")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := t.TempDir()
			files := filepath.Join(ref, "files")
			if err := os.Mkdir(files, 0700); err != nil {
				t.Fatal(err)
			}
			content := []byte("x")
			sum := sha256.Sum256(content)
			digest := hex.EncodeToString(sum[:])
			if err := os.WriteFile(filepath.Join(files, digest), content, 0600); err != nil {
				t.Fatal(err)
			}
			entry := `{"path":"/artifact","sha256":"` + digest + `"`
			if tc.present {
				entry += `,"bytes":` + tc.token
			}
			manifest := `{"entries":[` + entry + `}]}`
			if err := os.WriteFile(filepath.Join(ref, "MANIFEST.json"), []byte(manifest), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/store/testdata/frozen_detailed.py"), ref)
			cmd.Dir = repo
			want, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Python: %v\n%s", err, want)
			}
			result, problems, unreadable, goErr := VerifyFrozenDetailed(ref, nil)
			got := map[string]any{}
			if goErr != nil {
				got["error"], got["detail"] = "Exception", goErr.Error()
			} else {
				got["result"] = []any{result, problems, unreadable}
			}
			raw, _ := json.Marshal(got)
			var python, goValue any
			if err := json.Unmarshal(want, &python); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &goValue); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(goValue, python) {
				t.Fatalf("bytes=%s\nGo %s\nPython %s", tc.token, raw, bytes.TrimSpace(want))
			}
		})
	}
}
