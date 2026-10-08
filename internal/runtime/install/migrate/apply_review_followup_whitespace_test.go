package migrate

// apply_review_followup_whitespace_test.go holds the CRW-987 d4 cases: a receipt whose whitespace before a path or kind
// value is longer than the reader's 64 KiB window must still skip a container under that field instead of decoding it.

import (
	"io"
	"runtime/debug"
	"strings"
	"testing"
)

// migrateFollowupWriteArray writes a JSON array of about size bytes, one quoted run of whitespace per 64 KiB, followed by
// tail. The array holds only whitespace, so its memory cost is what the reader spends on it, not what the bytes hold.
func migrateFollowupWriteArray(w io.Writer, size int, tail string) error {
	const member = 64 << 10
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	for written := 0; written < size; written += member {
		if written > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\""); err != nil {
			return err
		}
		if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: member}, member); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\""); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]"+tail)
	return err
}

// CRW-987 d4: a path that is an array after more than 64 KiB of whitespace is skipped token by token. The judgement grows
// by the bytes of one token, the same as it grows for the same manifest with a plain path.
func TestMigrateFollowupLongWhitespaceBeforeAPathArrayIsNotHeld(t *testing.T) {
	const size = 32 << 20
	const gap = 128 << 10
	record := func(name string, container bool) string {
		return migrateReviewFollowupStreamRecord(t, t.TempDir(), name, func(w io.Writer) error {
			if _, err := io.WriteString(w, `{"artifactManifest":[{"path":`); err != nil {
				return err
			}
			if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: gap}, gap); err != nil {
				return err
			}
			if !container {
				_, err := io.WriteString(w, `"x/y.json","kind":"verdict"}]}`)
				return err
			}
			return migrateFollowupWriteArray(w, size, `,"kind":"verdict"}]}`)
		})
	}
	withBefore, withPeak, withOK := migrateFollowupJudgeLive(t, record("path array", true))
	withoutBefore, withoutPeak, withoutOK := migrateFollowupJudgeLive(t, record("path string", false))
	if !withOK || !withoutOK {
		t.Fatalf("a receipt whose manifest is an array is read whatever its entries hold: with %v, without %v", withOK, withoutOK)
	}
	grew := int64(withPeak-withBefore) - int64(withoutPeak-withoutBefore)
	if grew > size/8 {
		t.Errorf("the container under the path grew the judgement by %d bytes of a %d byte array", grew, size)
	}
}

// CRW-987 d4: the same for a kind that is an object after more than 64 KiB of whitespace.
func TestMigrateFollowupLongWhitespaceBeforeAKindObjectIsNotHeld(t *testing.T) {
	const size = 32 << 20
	const gap = 128 << 10
	record := func(name string, container bool) string {
		return migrateReviewFollowupStreamRecord(t, t.TempDir(), name, func(w io.Writer) error {
			if _, err := io.WriteString(w, `{"artifactManifest":[{"path":"x/y.json","kind":`); err != nil {
				return err
			}
			if _, err := io.CopyN(w, &migrateReviewFollowupSpaces{n: gap}, gap); err != nil {
				return err
			}
			if !container {
				_, err := io.WriteString(w, `"verdict"}]}`)
				return err
			}
			if _, err := io.WriteString(w, `{"blob":`); err != nil {
				return err
			}
			return migrateFollowupWriteArray(w, size, "}}]}")
		})
	}
	withBefore, withPeak, withOK := migrateFollowupJudgeLive(t, record("kind object", true))
	withoutBefore, withoutPeak, withoutOK := migrateFollowupJudgeLive(t, record("kind string", false))
	if !withOK || !withoutOK {
		t.Fatalf("a receipt whose manifest is an array is read whatever its entries hold: with %v, without %v", withOK, withoutOK)
	}
	grew := int64(withPeak-withBefore) - int64(withoutPeak-withoutBefore)
	if grew > size/8 {
		t.Errorf("the object under the kind grew the judgement by %d bytes of a %d byte object", grew, size)
	}
}

// CRW-987 d4 control: the exact 64 KiB boundary, and one byte past it, still read the path that follows the whitespace.
func TestMigrateFollowupWhitespaceBoundaryKeepsThePath(t *testing.T) {
	for _, gap := range []int{64 << 10, 64<<10 + 1, 1 << 20} {
		record := `{"artifactManifest":[{"path":` + strings.Repeat(" ", gap) + `"x/y.json","kind":"verdict"}]}`
		manifest, ok := migrateReviewFollowupDecodeManifest(strings.NewReader(record), int64(len(record)))
		if !ok || len(manifest) != 1 || manifest[0].Path != "x/y.json" || manifest[0].Kind != "verdict" {
			t.Errorf("whitespace of %d bytes: manifest %v ok %v", gap, manifest, ok)
		}
	}
}

// migrateFollowupJudgeLive judges the record at path with the collector running almost continuously. The heap the peak
// reads then holds what the judgement keeps live rather than the garbage a token-sized read leaves until the next collection,
// so the growth measured is the cost of the container and not of the collector's pacing.
func migrateFollowupJudgeLive(t *testing.T, path string) (before, peak uint64, ok bool) {
	t.Helper()
	prev := debug.SetGCPercent(1)
	defer debug.SetGCPercent(prev)
	return migrateReviewFollowupJudgePeakHeap(t, path)
}
