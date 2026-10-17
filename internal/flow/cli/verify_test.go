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

func TestFlowVerifySymlinkParentPaths(t *testing.T) {
	for _, tc := range []struct {
		name          string
		absolute      bool
		recordFromEnv bool
	}{
		{name: "relative checkout and record"},
		{name: "relative checkout record from env", recordFromEnv: true},
		{name: "absolute checkout and record", absolute: true},
		{name: "absolute checkout record from env", absolute: true, recordFromEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			other := filepath.Join(root, "other")
			for _, dir := range []string{filepath.Join(work, "repo", ".git"), filepath.Join(other, "repo", ".git"), filepath.Join(other, "child")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(other, "child"), filepath.Join(work, "link")); err != nil {
				t.Fatal(err)
			}
			receipt := `{"schema":"verification-record/1","result":"pass","digest":"sha256:fake","jobs":[]}`
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
pwd -P > "$CRW_TEST_VERIFY_CWD"
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
			cwdCapture := filepath.Join(root, "cwd")
			pathCapture := filepath.Join(root, "record-path")
			t.Setenv("CRW_CONFIG", config)
			t.Setenv("CRW_TEST_VERIFY_CWD", cwdCapture)
			t.Setenv("CRW_TEST_VERIFY_PATH", pathCapture)
			t.Chdir(root)
			checkout := "work/link/../repo"
			record := "work/link/../receipts/new/record.json"
			if tc.absolute {
				checkout = root + string(os.PathSeparator) + checkout
				record = root + string(os.PathSeparator) + record
			}
			args := []string{"verify", "--head", "HEAD", "--checkout", checkout}
			if tc.recordFromEnv {
				t.Setenv("CRW_VERIFY_RECORD", record)
			} else {
				args = append(args, "--record", record)
			}
			var out, errOut bytes.Buffer
			if code := Run(context.Background(), args, nil, &out, &errOut); code != 0 {
				t.Fatalf("verify: exit %d\n%s%s", code, &out, &errOut)
			}
			wantRepo, err := filepath.EvalSymlinks(filepath.Join(other, "repo"))
			if err != nil {
				t.Fatal(err)
			}
			cwdRaw, err := os.ReadFile(cwdCapture)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(cwdRaw)); got != wantRepo {
				t.Errorf("subprocess physical cwd = %q, want %q", got, wantRepo)
			}
			wantRecord := filepath.Join(wantRepo, ".git", "crw-verify", "verification-record-HEAD.json")
			pathRaw, err := os.ReadFile(pathCapture)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(pathRaw)); got != wantRecord {
				t.Errorf("subprocess record path = %q, want %q", got, wantRecord)
			}
			for _, path := range []string{wantRecord, filepath.Join(other, "receipts", "new", "record.json")} {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != receipt {
					t.Errorf("record at %s = %q, %v; want %s", path, raw, err, receipt)
				}
			}
			for _, missing := range []string{filepath.Join(work, "repo", ".git", "crw-verify"), filepath.Join(work, "receipts")} {
				if _, err := os.Lstat(missing); !os.IsNotExist(err) {
					t.Errorf("%s exists or errored otherwise: %v", missing, err)
				}
			}
		})
	}
}

func TestVerifyRecordWriteFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record")
	if err := os.Symlink(target, record); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordWrite(record, []byte("receipt")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "unchanged" {
		t.Fatalf("target = %q, %v", raw, err)
	}
	raw, err = os.ReadFile(record)
	if err != nil || string(raw) != "receipt" {
		t.Fatalf("record = %q, %v", raw, err)
	}
	info, err := os.Lstat(record)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the record path is still a symlink")
	}
}
