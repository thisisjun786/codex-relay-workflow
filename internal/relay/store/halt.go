package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the halt marker (CRW-848, decision CRW-847 section 83 item 1 as the issue body
// carries it): the durable fact that the relay saw the store damaged and stopped writing to it.
//
// On 2026-10-06 the daemon's reporting observation saw store_unreadable (disk I/O error 522,
// database disk image is malformed 11) between 13:44 and 13:52 only as an unmeasured notice, while
// the daemon and the CLI kept writing into the damaged store. The marker is what makes that stop:
// a read or a write that meets a failure of CorruptingFailure's class publishes S/corruption.json
// with ownership.Publish's durability, and every writable path refuses from then on while
// read-only commands and doctor still answer.
//
// Nothing is ever written into the store when the marker is set: the marker lives beside
// takeover.json and write-gate.lock, and the only files this file touches are the marker and the
// temporary file it is renamed from.

// HaltMarkerName is the marker's file name in the state directory S, beside takeover.json.
const HaltMarkerName = "corruption.json"

// Where the failure was seen, as the marker records it. A write is a statement the relay itself
// issued; an observation is the relay's own read of the store (the daemon's observation pass, the
// omission observer's reading).
const (
	HaltSiteWrite       = "write"
	HaltSiteObservation = "observation"
)

// HaltMarker is the marker's document: the detection time, the process that saw it, the failure and
// where it was seen, and the marker's own sequence.
type HaltMarker struct {
	DetectedAt string `json:"detectedAt"`
	PID        int    `json:"pid"`
	Command    string `json:"command"`
	Code       int    `json:"code"`
	Message    string `json:"message"`
	Site       string `json:"site"`
	Sequence   int    `json:"sequence"`
}

// HaltState is what the marker beside a store says: Present is whether one is there at all, Path
// is where it is (or would be), Marker its content when it decodes, and Detail why it could not be
// read. A marker that is present but unreadable or undecodable is Present with a Detail and a zero
// Marker: the halt is fail closed, because an operator's restore is what clears it, and a marker
// nobody can read is not evidence that the store is healthy.
type HaltState struct {
	Present bool
	Marker  HaltMarker
	Path    string
	Detail  string
}

// CorruptingCause is one detection: the failure's result code and message, and the site it was seen
// at. Site is filled by the caller (HaltSiteWrite or HaltSiteObservation).
type CorruptingCause struct {
	Code    int
	Message string
	Site    string
}

// haltMarkerPath is S/corruption.json for the store dbPath names, resolved the way the ownership
// fence names takeover.json (resolveLoosely): a --state reached through a symbolic link publishes
// and reads the marker beside the store the operator's other commands see, and a path that cannot
// be resolved is kept as spelled.
func haltMarkerPath(dbPath string) string {
	return filepath.Join(filepath.Dir(resolveLoosely(dbPath)), HaltMarkerName)
}

// HaltStateAt reads the marker beside the store dbPath names. It never fails: an absent marker is
// Present false, and a marker that cannot be read or decoded is Present true with the failure in
// Detail.
func HaltStateAt(dbPath string) HaltState {
	state := HaltState{Path: haltMarkerPath(dbPath)}
	// The directory entry is what decides presence, not what a read makes of it: a marker that is
	// a dangling symbolic link, or one that cannot be read at all, is a halt whose content is
	// unknown, never an absent marker.
	if _, err := os.Lstat(state.Path); errors.Is(err, os.ErrNotExist) {
		return state
	} else if err != nil {
		state.Present = true
		state.Detail = "the halt marker could not be examined: " + err.Error()
		return state
	}
	raw, err := os.ReadFile(state.Path)
	switch {
	case err != nil:
		state.Present = true
		state.Detail = "the halt marker could not be read: " + err.Error()
		return state
	}
	state.Present = true
	if err = json.Unmarshal(raw, &state.Marker); err != nil {
		state.Marker = HaltMarker{}
		state.Detail = "the halt marker could not be read: " + err.Error()
	}
	return state
}

// HaltRefusal is the refusal a writable path answers with while the marker exists: the new reason
// store_write_halted, whose detail names the marker file and the detection it holds (the issue
// fixes both halves of the detail). It is a *RefusedError, so dispatch renders it as every other
// refusal: {"error": "refused", "reason": "store_write_halted", "detail": ...} at exit 2.
func HaltRefusal(state HaltState) error {
	return &RefusedError{Reason: ReasonStoreWriteHalted, Detail: haltDetail(state)}
}

func haltDetail(state HaltState) string {
	if state.Detail != "" {
		return "the store is halted for writes: " + state.Detail + " (the marker is " + state.Path + ")"
	}
	marker := state.Marker
	return fmt.Sprintf("the store is halted for writes: %s records %s (code %d) seen at %s on %s by pid %d; it is cleared by hand, after a restore and a reconcile reading agree",
		state.Path, marker.Message, marker.Code, marker.Site, marker.DetectedAt, marker.PID)
}

// haltCommand is the process identity the marker records: the program and the command words that
// follow it, with every flag and the value that follows it dropped. A relay command line can carry
// a bearer token (--claim-token), and the marker outlives the process that wrote it and is read by
// whoever inspects the state directory, so the arguments are never copied in. What is left still
// names the process that saw the damage. It is bounded, so a hostile command line cannot grow the
// marker without limit. A variable so a test can pin the encoding of a hostile argument (JSON
// replaces invalid UTF-8).
var haltCommand = func() string { return haltCommandWords(os.Args, haltCommandLimit) }

// haltCommandLimit bounds the recorded identity.
const haltCommandLimit = 512

func haltCommandWords(argv []string, limit int) string {
	words := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			// A flag, and the separate word it takes as its value, are dropped whole. A value
			// written into the flag (--flag=value) goes with the flag.
			if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		words = append(words, arg)
	}
	line := strings.Join(words, " ")
	if len(line) > limit {
		line = line[:limit]
	}
	return line
}

// haltFault is a deterministic crash boundary seam for the marker's publication: production leaves
// it nil, and a test moves the file at a named point to prove no half-written marker is ever
// visible.
var haltFault func(string) error

// SetHaltFault installs the deterministic crash boundary the marker's publication runs (nil
// removes it). Tests only; production leaves it nil.
func SetHaltFault(fault func(string) error) { haltFault = fault }

// RecordHalt publishes S/corruption.json for the store dbPath names with ownership.Publish's
// durability: a temporary file in S, fsync, rename, then the directory fsync. It is the one
// marker-writing function: the daemon's write and observation paths, the CLI's refused open, the
// restore slice that must mark before it copies, and the clear command that removes it all go
// through this file.
//
// The marker holds the detection time, this process (pid and command), the failure's code and
// message, the site and the marker's own sequence. Nothing is written into the store.
//
// The sequence is best effort under a race: two processes that detect the damage at once read the
// same previous marker and publish the same next number, and the rename makes the last publication
// the one that stands. What survives is always one whole, decodable detection record (never a torn
// file, because the marker is renamed into place), which is what a reader needs; the sequence says
// how many detections one publisher counted, not how many happened.
//
// A cancelled context publishes nothing: the marker is a durable effect, and a pass that was told
// to stop writes none.
func RecordHalt(ctx context.Context, dbPath string, cause CorruptingCause) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := haltMarkerPath(dbPath)
	previous := HaltStateAt(dbPath)
	sequence := 1
	if previous.Present && previous.Detail == "" {
		sequence = previous.Marker.Sequence + 1
	}
	raw, err := json.Marshal(HaltMarker{
		DetectedAt: time.Now().UTC().Format(time.RFC3339Nano),
		PID:        os.Getpid(),
		Command:    haltCommand(),
		Code:       cause.Code,
		Message:    cause.Message,
		Site:       cause.Site,
		Sequence:   sequence,
	})
	if err != nil {
		return err
	}
	return publishHalt(path, raw)
}

// publishHalt is ownership.Publish's step order, kept verbatim: the temporary file is written,
// fsynced and closed, renamed onto the marker, and the directory is fsynced so the rename itself is
// durable. The order is what makes the marker atomic (a reader sees the old marker or the new one,
// never a partial file), and the restore slice and the clear command rely on the same shape.
//
// It is a local copy rather than a call into ownership because ownership.Publish publishes
// takeover.json and its publishFile is unexported; the store package cannot reach it without
// widening that package's API for a file ownership does not own.
func publishHalt(path string, raw []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".corruption-")
	if err != nil {
		return err
	}
	defer func() {
		if e := os.Remove(f.Name()); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if _, err = f.Write(raw); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = haltFaultPoint("temp-written"); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = haltFaultPoint("file-synced"); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err = haltFaultPoint("renamed"); err != nil {
		return err
	}
	if err = syncFile(filepath.Dir(path)); err != nil {
		return err
	}
	return haltFaultPoint("directory-synced")
}

func haltFaultPoint(point string) error {
	if haltFault == nil {
		return nil
	}
	return haltFault(point)
}
