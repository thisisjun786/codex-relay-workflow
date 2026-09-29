package inbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is the repository root, from this package's directory.
var repo = filepath.Join("..", "..", "..")

// cases are test_fence.py CASES and LEGACY_READBACK: the argv each golden entry was made from.
var cases = map[string][]string{
	"emit":                   {"--relationship", "relationship-1", "--generation", "1", "--outcome", "failed", "--turn-thread", "child-1", "--turn-id", "turn-1"},
	"ack":                    {"--event", "event-1", "--ack-turn", "parent-turn", "--ack-proof", "proof-1"},
	"fault-notification-ack": {"--notification", "notice-1", "--token", "token-1", "--ref", "receipt-1"},
	"supervisor-read":        {"--message", "message-1", "--turn", "supervisor-turn", "--proof", "proof-1", "--as", "supervisor-1"},
}

func golden(t *testing.T, command string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, "contract", "golden", "takeover-inbox", command+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Decision 25: the Go envelope of each golden's argv is the golden's bytes, and its payload
// digest is sha256 over the canonical arguments. Reverting any of the argparse-dest keying,
// default omission, canonical form or ID encoding changes these bytes.
func TestEnvelope_is_byte_identical_to_the_python_goldens(t *testing.T) {
	for command, argv := range cases {
		t.Run(command, func(t *testing.T) {
			entry, err := Envelope(command, argv)
			if err != nil {
				t.Fatal(err)
			}
			want := golden(t, command)
			if !bytes.Equal(entry.Raw, want) {
				t.Fatalf("envelope\n got %s\nwant %s", entry.Raw, want)
			}
			var decoded struct {
				Arguments   json.RawMessage `json:"arguments"`
				OperationID string          `json:"operationId"`
			}
			if err = json.Unmarshal(want, &decoded); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(decoded.Arguments)
			if entry.Digest != "sha256:"+hex.EncodeToString(sum[:]) || entry.ID != decoded.OperationID || entry.Command != command {
				t.Fatalf("entry %+v", entry)
			}
		})
	}
}

// The envelope's edge arguments against the live fence (inbox.envelope, one Python process):
// append lists, store_true, non-default and arbitrary-precision integers, explicit defaults
// (omitted), argparse's int spellings, empty strings, non-ASCII, U+2028, quotes, backslashes and
// control characters, percent-encoded identifiers, the 200-character boundary, and the usage
// rejections of an overlength identifier and of argv bytes that are not UTF-8 (Python's
// surrogate-escaped argv and its UnicodeEncodeError text).
func TestEnvelope_matches_the_python_fence_on_edge_arguments(t *testing.T) {
	type edge struct {
		Command string   `json:"command"`
		Argv    []string `json:"argv"` // base64 of each argv byte string
	}
	raw := func(command string, argv ...string) edge {
		e := edge{Command: command}
		for _, a := range argv {
			e.Argv = append(e.Argv, base64.StdEncoding.EncodeToString([]byte(a)))
		}
		return e
	}
	edges := []edge{
		raw("emit", "--relationship", "관계-\u2028-\"q\"-\\-\n-\x01-\x7f-é", "--generation", "123456789012345678901234567890", "--attempt", "3", "--outcome", "ready_for_review", "--turn-thread", "t", "--turn-id", "u", "--turn-status", "completed", "--artifact", "b", "--artifact", "a", "--manifest-ref", "", "--no-enqueue", "--continues-anchor", "x", "--supersedes-revision", "rev"),
		raw("emit", "--relationship", "r", "--generation", " ٣ ", "--attempt", "+01", "--outcome", "failed", "--turn-thread", "t", "--turn-id", "u", "--turn-status", "inProgress"),
		raw("emit", "--relationship", "r", "--generation", "-0", "--attempt=1_0", "--outcome", "interrupted", "--turn-thread=t", "--turn-id=-u"),
		raw("ack", "--event", "event/雪", "--ack-turn", "turn", "--ack-proof", "proof"),
		raw("ack", "--event", strings.Repeat("x", 196), "--ack-turn", "t", "--ack-proof", "p", "--reject", ""),
		raw("ack", "--event", strings.Repeat("x", 197), "--ack-turn", "t", "--ack-proof", "p"),
		raw("ack", "--event", "%", "--ack-turn", "t", "--ack-proof", "p", "--reject", "stale_generation"),
		raw("fault-notification-ack", "--notification", "n ü\t", "--token", "tok", "--ref", "r"),
		raw("ack", "--event", "event-\xff", "--ack-turn", "t", "--ack-proof", "p"),
		raw("ack", "--event", "e", "--ack-turn", "a\xff\xfeb", "--ack-proof", "p"),
		raw("emit", "--relationship", "\xed\xa0\x80", "--generation", "1", "--outcome", "failed", "--turn-thread", "t", "--turn-id", "u"),
		raw("emit", "--relationship", "r", "--generation", "1", "--outcome", "failed", "--turn-thread", "t", "--turn-id", "\n\"\x01\xe2\x82", "--artifact", "ok", "--artifact", "x\xc0"),
		raw("emit", "--relationship", "r", "--generation", "1", "--outcome", "failed", "--turn-thread", "t", "--turn-id", "u", "--artifact", "ok", "--artifact", "x\xc0\x80y\xff"),
	}
	input, err := json.Marshal(edges)
	if err != nil {
		t.Fatal(err)
	}
	script := `import base64, json, os, sys
from codex_session_relay import cli, inbox
parser = cli.build_parser()
out = []
for case in json.load(sys.stdin):
    argv = [case["command"]] + [os.fsdecode(base64.b64decode(a)) for a in case["argv"]]
    try:
        out.append({"raw": base64.b64encode(inbox.canonical(inbox.envelope(parser, parser.parse_args(argv)))).decode()})
    except ValueError as error:
        out.append({"usage": str(error)})
json.dump(out, sys.stdout)
`
	python := exec.Command(filepath.Join(repo, ".venv", "bin", "python"), "-c", script)
	python.Stdin = bytes.NewReader(input)
	python.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := python.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("%v: %s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	var want []struct{ Raw, Usage string }
	if err = json.Unmarshal(output, &want); err != nil || len(want) != len(edges) {
		t.Fatalf("%v: %s", err, output)
	}
	for i, e := range edges {
		var argv []string
		for _, a := range e.Argv {
			b, _ := base64.StdEncoding.DecodeString(a)
			argv = append(argv, string(b))
		}
		entry, err := Envelope(e.Command, argv)
		var usage *UsageError
		switch {
		case want[i].Usage != "":
			if !errors.As(err, &usage) || usage.Detail != want[i].Usage {
				t.Errorf("case %d %q: usage %v, python %q", i, argv, err, want[i].Usage)
			}
		case err != nil:
			t.Errorf("case %d %q: %v", i, argv, err)
		default:
			expected, _ := base64.StdEncoding.DecodeString(want[i].Raw)
			if !bytes.Equal(entry.Raw, expected) {
				t.Errorf("case %d %q:\n got %s\nwant %s", i, argv, entry.Raw, expected)
			}
		}
	}
}
