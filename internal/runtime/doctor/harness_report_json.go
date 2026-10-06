package doctor

import "bytes"

// RenderHarnessReportJSON is the harness report's --json document: JSON.stringify(report, null, 2)
// plus the trailing newline the oracle's command writes (cli.ts:83-84), byte for byte what
// harnessRunWriteJSON writes (harness_run.go). It is the exported seam the dev-only
// differential-fuzz target compares against the oracle, so the JSON the port prints and the JSON
// a fuzz case compares cannot drift; it adds no behaviour of its own.
func RenderHarnessReportJSON(report HarnessReport) ([]byte, error) {
	var buffer bytes.Buffer
	if err := harnessRunWriteJSON(&buffer, report); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
