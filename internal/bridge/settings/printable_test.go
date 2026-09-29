package settings

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// The table is str.isprintable() of every code point as the CPython it was read from answers it,
// checked against that interpreter when it is the one on PATH (a CPython with another Unicode
// database answers another question, so the check is skipped there).
func TestPrintableIsCPython314sIsprintable(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	out, err := exec.Command(python, "-c", `
import json, sys, unicodedata
json.dump({"version": unicodedata.unidata_version,
           "printable": "".join("1" if not 0xd800 <= c <= 0xdfff and chr(c).isprintable() else "0"
                                for c in range(0x110000))}, sys.stdout)
`).Output()
	if err != nil {
		t.Fatal(err)
	}
	var answer struct{ Version, Printable string }
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Version != printableVersion {
		t.Skipf("python3 has Unicode %s, the table CPython 3.14's %s", answer.Version, printableVersion)
	}
	differ := 0
	for c := range rune(0x110000) {
		if Printable(c) != (answer.Printable[c] == '1') {
			if differ++; differ <= 5 {
				t.Errorf("U+%04X: Printable %v, CPython %s says %c", c, Printable(c), answer.Version, answer.Printable[c])
			}
		}
	}
	if differ > 0 {
		t.Fatalf("%d code points differ; regenerate with `go generate ./internal/bridge/settings`", differ)
	}
}
