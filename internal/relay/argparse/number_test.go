package argparse

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Accepted values are compared as typed numbers, not just successful parses.
// This catches a converter which accepts Unicode/large integers but truncates them.
func Test24NumericPythonBytes(t *testing.T) {
	values := []string{"9999999999999999999999999", "٣", "١٢", "𝟡", "１_٢", "_1", "1_", "1__0", "1_0", " 2 ", "+3", "-0", "0x1p3", "nan", "+nan", "-NaN", "inf", "INFINITY", "-inf", "1e400", "1e-400", "١٢.٣", ".5", "1.", "1.e2", "1_e2", "1e_2", "\u20031\u2003", "\x1c1", "1\x00", "²", "−1", "", strings.Repeat("9", 4300), strings.Repeat("9", 4301), strings.Repeat("0", 4301)}
	request, _ := json.Marshal(values)
	root, _ := filepath.Abs("../../..")
	script := `import json,sys,struct
values=json.load(sys.stdin)
for kind in (int,float):
 for text in values:
  try:
   value=kind(text)
   print(str(value) if kind is int else struct.pack('>d',value).hex() if value==value else 'nan')
  except ValueError: print('invalid')
`
	cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script)
	cmd.Stdin = bytes.NewReader(request)
	want, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle: %v %s", err, want)
	}
	var got strings.Builder
	for _, kind := range []string{"int", "float"} {
		for _, value := range values {
			text := "invalid"
			if kind == "int" {
				if n, ok := ParseInt(value); ok {
					text = n.String()
				}
			} else if n, ok := ParseFloat(value); ok {
				if math.IsNaN(n) {
					text = "nan"
				} else {
					text = strconv.FormatUint(math.Float64bits(n), 16)
					text = strings.Repeat("0", 16-len(text)) + text
				}
			}
			got.WriteString(text + "\n")
		}
	}
	if got.String() != string(want) {
		t.Fatalf("numeric byte diff\nGo=%s\nPython=%s", got.String(), want)
	}
	parsed := Parse("merge-evidence", []string{"--repository=o/r", "--pull-request=9999999999999999999999999"})
	n, ok := parsed.Numbers["pull-request"].(*big.Int)
	if parsed.Message != "" || !ok || n.String() != values[0] {
		t.Fatalf("untyped or truncated parse: %+v", parsed)
	}
}
