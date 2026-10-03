package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test28FrozenByteCountExactPythonParity(t *testing.T) {
	t.Parallel()
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
			result, problems, unreadable, goErr := VerifyFrozenDetailed(context.Background(), ref, nil)
			got := map[string]any{}
			if goErr != nil {
				got["error"], got["detail"] = "Exception", goErr.Error()
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
