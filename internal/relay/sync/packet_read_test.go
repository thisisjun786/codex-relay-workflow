package sync

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAPacketFileThatIsNotJSONIsAUsageError: packet-check refuses a packet file it cannot read as
// JSON with exit 4 and a usage error naming the file and encoding/json's reason.
func TestAPacketFileThatIsNotJSONIsAUsageError(t *testing.T) {
	binary := builtBinary(t)
	dir := t.TempDir()
	packet := filepath.Join(dir, "packet.json")
	if err := os.WriteFile(packet, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke := exec.Command(binary, "relay", "--state", filepath.Join(dir, "state"), "packet-check", "--packet", packet, "--record", packet)
	var stdout bytes.Buffer
	invoke.Stdout = &stdout
	if code := processCode(invoke.Run()); code != 4 {
		t.Fatalf("exit %d: %s", code, stdout.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	detail, _ := answer["detail"].(string)
	if answer["error"] != "usage" || !strings.Contains(detail, packet) || !strings.Contains(detail, "invalid character") {
		t.Fatalf("%v", answer)
	}
}
