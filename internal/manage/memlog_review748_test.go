package manage

import (
	"strings"
	"testing"
	"time"
)

// TestMemlogReview748ValuelessCredentialOptionDoesNotHideTheNext pins the credential exposure
// of PR #748: "--auth-no-challenge" names a credential, so the old loop masked the word after
// it and skipped judging that word. "--password" was therefore read as a value, never as a key,
// and "hunter2" reached the record in the clear.
func TestMemlogReview748ValuelessCredentialOptionDoesNotHideTheNext(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 7, ppid: 1, rss: 4096,
		args: []string{"wget", "--auth-no-challenge", "--password", "hunter2", "https://example.invalid"}}})
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if len(record.Top) != 1 {
		t.Fatalf("top holds %d entries, want 1", len(record.Top))
	}
	cmd := record.Top[0].Cmd
	if strings.Contains(cmd, "hunter2") {
		t.Errorf("the recorded cmd %q still carries the credential", cmd)
	}
	if want := "wget --auth-no-challenge *** *** https://example.invalid"; cmd != want {
		t.Errorf("the recorded cmd is %q, want %q", cmd, want)
	}
}

// TestMemlogReview748ChainedKeys pins the chain: a word masked as a valueless option's value is
// still judged as a key itself, so "--password" masks "s3cret" and the NAME=VALUE option after
// it masks its own value. A word that names no credential survives.
func TestMemlogReview748ChainedKeys(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 7, ppid: 1, rss: 4096,
		args: []string{"tool", "--token", "--password", "s3cret", "--api-key=k1", "plain"}}})
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if len(record.Top) != 1 {
		t.Fatalf("top holds %d entries, want 1", len(record.Top))
	}
	cmd := record.Top[0].Cmd
	for _, secret := range []string{"s3cret", "k1"} {
		if strings.Contains(cmd, secret) {
			t.Errorf("the recorded cmd %q still carries %q", cmd, secret)
		}
	}
	if want := "tool --token *** *** --api-key=*** plain"; cmd != want {
		t.Errorf("the recorded cmd is %q, want %q", cmd, want)
	}
}

// TestMemlogReview748KeepsACleanLineByteForByte is the contrast: a command line that names no
// credential is recorded exactly as the kernel kept it, so the wider decision did not start
// masking ordinary arguments.
func TestMemlogReview748KeepsACleanLineByteForByte(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	args := []string{"/usr/bin/svc", "--socket", "/run/x", "--dbpath", "/data/go", "run"}
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 7, ppid: 1, rss: 4096, args: args}})
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if len(record.Top) != 1 {
		t.Fatalf("top holds %d entries, want 1", len(record.Top))
	}
	if want := strings.Join(args, " "); record.Top[0].Cmd != want {
		t.Errorf("the recorded cmd is %q, want %q", record.Top[0].Cmd, want)
	}
}

// TestMemlogReview748KeepsAnAlreadyMaskedValueMasked pins that judging a word again never
// un-masks part of a value an earlier argument already redacted. The whole word after
// "--token" is that option's value, so it stays "***" even though it reads like a NAME=VALUE
// pair whose name is credential-shaped.
func TestMemlogReview748KeepsAnAlreadyMaskedValueMasked(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 7, ppid: 1, rss: 4096,
		args: []string{"svc", "--token", "secretCustomer42=abc"}}})
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if len(record.Top) != 1 {
		t.Fatalf("top holds %d entries, want 1", len(record.Top))
	}
	cmd := record.Top[0].Cmd
	for _, leak := range []string{"secretCustomer42", "abc"} {
		if strings.Contains(cmd, leak) {
			t.Errorf("the recorded cmd %q still carries %q", cmd, leak)
		}
	}
	if want := "svc --token ***"; cmd != want {
		t.Errorf("the recorded cmd is %q, want %q", cmd, want)
	}
}
