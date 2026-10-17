package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake crw-dev writes its record relative to the checkout, just as a real subprocess does.
// A relative checkout must not make that path resolve a second time inside the checkout (CRW-1620).
func TestFlowVerifyLocalPaths(t *testing.T) {
	for _, tc := range []struct {
		name, checkout string
		absoluteRecord bool
		recordFromEnv  bool
	}{
		{name: "relative checkout and record", checkout: "relative"},
		{name: "relative checkout absolute record", checkout: "relative", absoluteRecord: true},
		{name: "absolute checkout relative record", checkout: "absolute"},
		{name: "absolute checkout and record", checkout: "absolute", absoluteRecord: true},
		{name: "relative checkout environment record", checkout: "relative", recordFromEnv: true},
		{name: "default checkout environment record", recordFromEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			const receipt = `{"schema":"verification-record/1","result":"pass","digest":"sha256:fake","jobs":[]}`
			bin := filepath.Join(root, "crw-dev")
			body := `#!/bin/sh
set -eu
[ "$1 $2" = "ci local" ]
shift 2
record=""
while [ "$#" -gt 0 ]; do
  case "$1" in --record) record="$2"; shift;; esac
  shift
done
printf '%s\n' "$record" > "$CRW_TEST_VERIFY_PATH"
mkdir -p "$(dirname "$record")"
printf '%s' '` + receipt + `' > "$record"
`
			if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "config.json")
			if err := os.WriteFile(config, []byte(fmt.Sprintf(`{"verifier":{"kind":"local","crw_dev":%q}}`, bin)), 0o600); err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(root, "record-path")
			t.Setenv("CRW_CONFIG", config)
			t.Setenv("CRW_TEST_VERIFY_PATH", capture)
			t.Chdir(root)
			args := []string{"verify", "--head", "HEAD"}
			switch tc.checkout {
			case "relative":
				args = append(args, "--checkout", "repo")
			case "absolute":
				args = append(args, "--checkout", repo)
			default:
				t.Chdir(repo)
			}
			record := filepath.Join("receipts", "record.json")
			wantRecord, err := filepath.Abs(record)
			if err != nil {
				t.Fatal(err)
			}
			if tc.absoluteRecord {
				record = wantRecord
			}
			if tc.recordFromEnv {
				t.Setenv("CRW_VERIFY_RECORD", record)
			} else {
				args = append(args, "--record", record)
			}
			var out, errOut bytes.Buffer
			if code := Run(context.Background(), args, nil, &out, &errOut); code != 0 {
				t.Fatalf("verify: exit %d\n%s%s", code, &out, &errOut)
			}
			path, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := strings.TrimSpace(string(path)), filepath.Join(repo, ".git", "crw-verify", "verification-record-HEAD.json"); got != want {
				t.Errorf("subprocess record path = %q, want %q", got, want)
			}
			for _, path := range []string{strings.TrimSpace(string(path)), wantRecord} {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != receipt {
					t.Errorf("record at %s = %q, %v; want %s", path, raw, err, receipt)
				}
			}
		})
	}
}
