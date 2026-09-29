//go:build parity

package delivery

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateMarkerPreflight = flag.Bool("update-marker-preflight", false, "rewrite testdata/marker_preflight.json from live Python")

// Every marker preflight case, run by the live Python console script and the Go CLI side by
// side, each over its own home: stdout, exit, the marker files and the state directory must
// agree. With -update-marker-preflight it records Python's answers as the golden the default
// suite checks the Go CLI against (TestCLI_marker_preflight_answers_what_python_answers).
func TestCLI_marker_preflight_parity_with_live_python(t *testing.T) {
	captured := map[string]markerOutcome{}
	for _, c := range markerCases() {
		t.Run(c.name, func(t *testing.T) {
			python := runMarkerCase(t, markerSide(t, true), true, c)
			gosd := runMarkerCase(t, markerSide(t, false), false, c)
			captured[c.name] = python
			if !equalOutcome(python, gosd) {
				t.Errorf("python:\n%s\ngo:\n%s", outcomeText(python), outcomeText(gosd))
			}
			if refused := strings.HasSuffix(c.name, "owned-by-the-other-runtime") || strings.HasSuffix(c.name, "draining"); refused &&
				(python.Exit != 2 || len(python.Markers) > 1 || python.StateChanged || !strings.Contains(python.Stdout, `"reason": "store_owned_by_other"`)) {
				t.Errorf("python did not refuse before any marker write:\n%s", outcomeText(python))
			}
		})
	}
	if !*updateMarkerPreflight {
		return
	}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	mustDo(t, encoder.Encode(captured))
	mustDo(t, os.WriteFile(filepath.Join("testdata", "marker_preflight.json"), raw.Bytes(), 0o644))
}
