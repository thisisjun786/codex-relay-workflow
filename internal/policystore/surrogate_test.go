package policystore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadOpensASurrogateEscapedPath is the Codex review finding: the record stores a policy path
// whose bytes are not UTF-8 as a surrogate escape, and the launcher opens it as that byte.
func TestReadOpensASurrogateEscapedPath(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file name with one byte that is not valid UTF-8: the byte 0x80 stands on its own.
	raw := append([]byte("policy"), 0x80)
	raw = append(raw, []byte(".json")...)
	name := filepath.Join(root, string(raw))
	if err := os.WriteFile(name, []byte(policyText), 0o644); err != nil {
		t.Skipf("this filesystem refuses a non-UTF-8 name: %v", err)
	}
	// The record carries that byte the way a Python writer does: as the lone surrogate U+DC80,
	// spelled in JSON as an escape.
	// Go converts an invalid rune to U+FFFD in a string, so the WTF-8 spelling is built from the
	// bytes the lone surrogate is encoded as (ED B2 80).
	surrogate := string([]byte{0xED, 0xB2, 0x80})
	escaped := filepath.Join(root, "policy"+surrogate+".json")
	document := "{\n" +
		"  \"recordVersion\": 2,\n" +
		"  \"owner\": \"plugin\",\n" +
		"  \"serverName\": \"codex-thread-bridge\",\n" +
		"  \"bridgeExecutable\": \"/usr/local/bin/codex-thread-bridge\",\n" +
		"  \"args\": [],\n" +
		"  \"executionPolicy\": {\"path\": \"" + jsonEscaped(escaped) + "\", \"digest\": \"" + digestOf(policyText) + "\"}\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(codexHome, "crw-bridge-mcp.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	reading := Read(Locate(envOf(map[string]string{"HOME": root, "CODEX_HOME": codexHome})))
	if reading.State != Registered {
		t.Fatalf("the surrogate-escaped path was not opened: %+v", reading)
	}
	if reading.Digest != digestOf(policyText) {
		t.Fatalf("digest = %q", reading.Digest)
	}
}

// jsonEscaped spells a Go string the way a Python writer spells a path whose bytes are not UTF-8:
// the byte 0x80 stands in the string as the WTF-8 encoding of the lone surrogate U+DC80 (ED B2 80),
// and the record carries it as the JSON escape \udc80. The scan is over bytes, because Go's range
// rejects a surrogate encoding and would report three replacement characters instead.
func jsonEscaped(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == 0xED && i+2 < len(value) && value[i+1] == 0xB2 && value[i+2] == 0x80 {
			out = append(out, []byte(bs+"udc80")...)
			i += 2
			continue
		}
		out = append(out, value[i])
	}
	return string(out)
}

// bs is the escape character a JSON surrogate escape begins with.
const bs = "\\"
