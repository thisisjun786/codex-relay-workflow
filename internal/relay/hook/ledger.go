package hook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

type Slot struct{ Day, ID string }

// errUnencodable is os.fsencode's UnicodeEncodeError for a journal root holding a lone surrogate
// other than U+DC80..U+DCFF: Python's writers catch it with OSError and write nothing.
var errUnencodable = errors.New("the journal root holds a surrogate os.fsencode cannot encode")

func NewSlot() (Slot, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Slot{}, err
	}
	return Slot{time.Now().UTC().Format("20060102"), hex.EncodeToString(raw)}, nil
}
func (s Slot) Name() string { return s.Day + "/" + s.ID + ".json" }
func now() string           { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// claimWriteKey is a test seam at the actual write syscall boundary. Creation,
// ownership, close and torn-file handling always remain on the production path.
type claimWriteKey struct{}
type claimWriteFunc func(*os.File, []byte) (int, error)

// createOnce never removes a claim, including a short write. Absence of its outcome
// is the durable indication that its owner did not finish; it is not a replay lease.
func createOnce(ctx context.Context, path string, document Object, keepTorn bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	raw := RecordBytes(document)
	write := f.Write
	if injected, ok := ctx.Value(claimWriteKey{}).(claimWriteFunc); ok {
		write = func(raw []byte) (int, error) { return injected(f, raw) }
	}
	_, err = write(raw)
	err = errors.Join(err, f.Close())
	if err != nil && !keepTorn {
		err = errors.Join(err, os.Remove(path))
	}
	return true, err
}
func ClaimEvent(ctx context.Context, config Object, key string, identity, stop Object, slot Slot, host string) (string, any) {
	root := pyjson.Text(config.Get("journalRoot"))
	base := Object{{Key: "ledgerVersion", Value: int64(LedgerVersion)}, {Key: "eventKey", Value: key}, {Key: "sessionId", Value: stop.Get("session_id")}, {Key: "turnId", Value: stop.Get("turn_id")}, {Key: "stopHookActive", Value: stop.Get("stop_hook_active")}, {Key: "answerItem", Value: identity.Get("answerItem")}, {Key: "claimedAt", Value: now()}}
	if host != "" {
		if ctx.Err() != nil {
			return "unarbitrated", nil
		}
		if err := os.MkdirAll(host, 0777); err != nil {
			return "unarbitrated", nil
		}
		document := append(Object{}, base...).Set("claimedBy", Object{{Key: "pid", Value: os.Getpid()}, {Key: "journalRoot", Value: nullable(root)}, {Key: "attemptRow", Value: slot.Name()}})
		// Python _arbitrate suppresses a post-O_EXCL write failure, keeps the
		// inode, and grants ownership. Deleting or treating it as unowned would
		// permit replay or silently change Python's hold behavior.
		created, err := createOnce(ctx, filepath.Join(host, key+".json"), document, true)
		if !created {
			if errors.Is(err, os.ErrExist) {
				return Duplicate, strings.Join(HostLedgerParts, "/") + "/" + key + ".json"
			}
			return "unarbitrated", nil
		}
	}
	if root == "" {
		return "unclaimable", nil
	}
	rootFS, encoded := pyvalue.FSEncode(root)
	if !encoded {
		return "claim_failed", nil
	}
	directory := filepath.Join(rootFS, LedgerDirectory)
	if ctx.Err() != nil {
		return "claim_failed", nil
	}
	if err := os.MkdirAll(directory, 0777); err != nil {
		return "claim_failed", nil
	}
	// The host ledger is named as Python holds a path from the environment: os.fsdecode's str.
	document := base.Set("claimedBy", Object{{Key: "pid", Value: os.Getpid()}, {Key: "attemptRow", Value: slot.Name()}, {Key: "hostLedger", Value: nullable(pyvalue.FSDecode(host))}})
	// claim_event likewise returns ACCEPTED after a failed write, never unlinking.
	created, err := createOnce(ctx, filepath.Join(directory, key+".json"), document, true)
	name := LedgerDirectory + "/" + key + ".json"
	if !created {
		if errors.Is(err, os.ErrExist) {
			return "duplicate", name
		}
		return "claim_failed", nil
	}
	return "accepted", name
}
func Journal(ctx context.Context, config, record Object, slot Slot) (string, error) {
	policy := pyjson.Text(config.Get("journalPolicy"))
	if policy == "no_journal" {
		return "", nil
	}
	if policy == "faults_only" && (record.Get("adapterOutcome") == "guard_answered" || record.Get("adapterOutcome") == "duplicate_invocation") && (record.Get("acceptance") == "accepted" || record.Get("acceptance") == "duplicate") {
		return "", nil
	}
	root := pyjson.Text(config.Get("journalRoot"))
	if root == "" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, encoded := pyvalue.FSEncode(root)
	if !encoded {
		return "", errUnencodable
	}
	directory := filepath.Join(root, slot.Day)
	if err := os.MkdirAll(directory, 0777); err != nil {
		return "", err
	}
	path := filepath.Join(directory, slot.ID+".json")
	_, err := createOnce(ctx, path, record, false)
	if err != nil {
		return "", err
	}
	return path, nil
}
func RecordOutcome(ctx context.Context, config Object, key string, record Object, row any) error {
	root := pyjson.Text(config.Get("journalRoot"))
	if root == "" || key == "" {
		return nil
	}
	root, encoded := pyvalue.FSEncode(root)
	if !encoded {
		return errUnencodable
	}
	policy := config.Get("journalPolicy")
	if !pyvalue.Truthy(policy) {
		policy = EveryInvocation
	}
	out := Object{{Key: "ledgerVersion", Value: int64(LedgerVersion)}, {Key: "eventKey", Value: key}, {Key: "sessionId", Value: record.Get("sessionId")}, {Key: "turnId", Value: record.Get("turnId")}, {Key: "journalPolicy", Value: policy}, {Key: "adapterOutcome", Value: record.Get("adapterOutcome")}, {Key: "guardDecision", Value: record.Get("guardDecision")}, {Key: "guardState", Value: record.Get("guardState")}, {Key: "held", Value: record.Get("held")}, {Key: "attemptRow", Value: row}, {Key: "at", Value: now()}}
	_, err := createOnce(ctx, filepath.Join(root, LedgerDirectory, key+OutcomeSuffix), out, false)
	return err
}
