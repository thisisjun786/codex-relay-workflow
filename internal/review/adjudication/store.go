// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/store/adjudicationstore.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package adjudication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"

	"golang.org/x/sys/unix"
)

const maxRecordBytes = 1 << 20
const maxLedgerBytes = 64 << 20

func decode(data []byte, target any) error {
	if len(data) > maxRecordBytes {
		return fmt.Errorf("JSON exceeds %d bytes", maxRecordBytes)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	// Compare JSON values with the canonical typed shape. This catches omitted or
	// null scalars that encoding/json silently coerces, while retaining nullable fields.
	var raw, canonical any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		return err
	}
	if !reflect.DeepEqual(raw, canonical) {
		return fmt.Errorf("JSON must preserve the canonical field shape and nullability")
	}
	return nil
}

// Read parses a stable JSONL snapshot, refusing corruption, torn tails and inconsistent truth.
func Read(input io.Reader) ([]Record, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxLedgerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxLedgerBytes {
		return nil, fmt.Errorf("ledger exceeds %d bytes", maxLedgerBytes)
	}
	records := []Record{}
	if len(data) == 0 {
		return records, nil
	}
	if data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("ledger has a torn tail; no bytes discarded")
	}
	for i, line := range bytes.Split(data[:len(data)-1], []byte{'\n'}) {
		var r Record
		if err := decode(line, &r); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		records = append(records, r)
	}
	_, err = replay(records)
	return records, err
}

// Append validates a whole correction batch under a nonblocking writer lock before appending.
// It never truncates history. Crash-torn writes fail closed on the next Read/Append.
func Append(file string, records ...Record) error {
	if len(records) == 0 {
		return fmt.Errorf("empty append batch")
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR|os.O_APPEND|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ledger must be a regular file")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("ledger lock: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	previous, err := Read(f)
	if err != nil {
		return err
	}
	if _, err := replay(append(previous, records...)); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, r := range records {
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if len(data) > maxRecordBytes {
			return fmt.Errorf("record exceeds %d bytes", maxRecordBytes)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	info, err = f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size()+int64(buf.Len()) > maxLedgerBytes {
		return fmt.Errorf("ledger must be a regular file within size limit")
	}
	if n, err := f.Write(buf.Bytes()); err != nil {
		return err
	} else if n != buf.Len() {
		return io.ErrShortWrite
	}
	return f.Sync()
}
