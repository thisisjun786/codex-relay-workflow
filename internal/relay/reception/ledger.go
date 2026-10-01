package reception

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"golang.org/x/sys/unix"
)

// LedgerError is an unusable ledger or an application with no accepted actionable check.
type LedgerError struct{ Detail string }

func (e *LedgerError) Error() string { return e.Detail }
func ledgerError(format string, args ...any) error {
	return &LedgerError{Detail: fmt.Sprintf(format, args...)}
}
func EmptyLedger(receiver string) Obj {
	return O("version", 1, "receiver", receiver, "answered", Obj{}, "assignments", Obj{})
}

// WithLedgerLock holds the permanent sidecar across the complete read/decision/write.
func WithLedgerLock(path string, run func() error) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unix.Flock(int(f.Fd()), unix.LOCK_UN)) }()
	return run()
}
func LoadLedger(path, receiver string) (Obj, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return EmptyLedger(receiver), nil
	}
	if err != nil {
		return nil, ledgerError("the reception ledger at %s could not be read: %s", path, err)
	}
	if problem := JSONReaderDepthProblem(raw); problem != "" {
		return nil, ledgerError("the reception ledger at %s could not be read: RecursionError: %s", path, problem)
	}
	decoded, err := registry.DecodeJSON(string(raw))
	if err != nil {
		return nil, ledgerError("the reception ledger at %s could not be read: JSONDecodeError: %s", path, err)
	}
	ledger, ok := evidence.Object(decoded)
	version, _ := evidence.PyInt(Get(ledger, "version"))
	_, answers := evidence.Object(Get(ledger, "answered"))
	_, assignments := evidence.Object(Get(ledger, "assignments"))
	if !ok || version != 1 || !answers || !assignments {
		return nil, ledgerError("the file at %s is not a version 1 reception ledger", path)
	}
	if Get(ledger, "receiver") != receiver {
		return nil, ledgerError("the reception ledger at %s belongs to %s, not %s; another receiver's answers are not this one's", path, evidence.Repr(Get(ledger, "receiver")), evidence.Repr(receiver))
	}
	if p := entryProblem(ledger); p != "" {
		return nil, ledgerError("the reception ledger at %s is damaged: %s; a ledger that cannot say what was answered is not read through", path, p)
	}
	return ledger, nil
}
func entryProblem(ledger Obj) string {
	answered, _ := evidence.Object(Get(ledger, "answered"))
	for _, f := range answered {
		entry := f.Value
		_, object := evidence.Object(entry)
		_, applied := Get(entry, "applied").(bool)
		_, told := Get(entry, "toldToAct").(bool)
		if !object || evidence.TypeName(Get(entry, "contentDigest")) != "str" || !present(Get(entry, "contentDigest")) || !slices.Contains([]string{"accepted", "refused", "unavailable"}, str(Get(entry, "disposition"))) || !applied || !told {
			return "answered entry " + evidence.Repr(f.Key) + " is not a content digest, a disposition, whether a check said act and whether it was applied"
		}
		if Get(entry, "applied") == true && (Get(entry, "toldToAct") != true || Get(entry, "disposition") != "accepted") {
			return "answered entry " + evidence.Repr(f.Key) + " says applied for a packet it does not hold as accepted and told to act on"
		}
	}
	assignments, _ := evidence.Object(Get(ledger, "assignments"))
	for _, f := range assignments {
		entry := f.Value
		ok := slices.Contains(modes, str(Get(entry, "mode")))
		for _, k := range []string{"workflow", "messageId", "dispatchRequestId"} {
			if evidence.TypeName(Get(entry, k)) != "str" || !present(Get(entry, k)) {
				ok = false
			}
		}
		if !ok {
			return "assignment entry " + evidence.Repr(f.Key) + " is not an execution mode, a workflow, the message id of the assignment and a dispatch id"
		}
	}
	return ""
}
func RecordAnswer(ledger *Obj, packet, answer any) bool {
	changed := false
	identifier := str(Get(answer, "messageId"))
	state := Get(Get(answer, "repeat"), "state")
	accepted, told := Get(answer, "disposition") == "accepted", Get(answer, "act") == true
	answered, _ := evidence.Object(Get(*ledger, "answered"))
	if state == "first" {
		Set(&answered, identifier, O("contentDigest", Get(Get(answer, "repeat"), "contentDigest"), "disposition", Get(answer, "disposition"), "applied", false, "toldToAct", told))
		changed = true
	} else if state == "replay" {
		entry, _ := evidence.Object(Get(answered, identifier))
		if accepted && Get(entry, "disposition") != "accepted" {
			Set(&entry, "disposition", "accepted")
			changed = true
		}
		if told && Get(entry, "toldToAct") != true {
			Set(&entry, "toldToAct", true)
			changed = true
		}
		Set(&answered, identifier, entry)
	}
	Set(ledger, "answered", answered)
	region, record := Get(packet, "envelope"), Get(answer, "record")
	rid, dispatch := str(Get(record, "relationId")), Get(record, "tenureDispatchRequestId")
	assignments, _ := evidence.Object(Get(*ledger, "assignments"))
	recorded := Get(assignments, rid)
	if accepted && (state == "first" || state == "replay") && Get(region, "direction") == "parent_to_child" && Get(region, "purpose") == "assignment" && rid != "" && truth(dispatch) && (recorded == nil || !equal(Get(recorded, "dispatchRequestId"), dispatch)) {
		policy := Get(packet, "policy")
		Set(&assignments, rid, O("mode", Get(policy, "mode"), "workflow", Get(policy, "workflow"), "messageId", identifier, "dispatchRequestId", dispatch))
		Set(ledger, "assignments", assignments)
		changed = true
	}
	return changed
}
func RecordApplied(ledger *Obj, packet any) (Obj, error) {
	if e := Check(packet); e != nil {
		return nil, e
	}
	answered, _ := evidence.Object(Get(*ledger, "answered"))
	repeat := Repeat(packet, answered)
	id := str(Get(repeat, "messageId"))
	switch Get(repeat, "state") {
	case "first":
		return nil, ledgerError("message %s was never checked against this ledger, so there is no accepted answer to record as applied; run the check first", id)
	case "collision":
		return nil, ledgerError("message %s cannot be recorded as applied: %s", id, Get(repeat, "reason"))
	}
	entry, _ := evidence.Object(Get(answered, id))
	if Get(entry, "disposition") != "accepted" {
		return nil, ledgerError("message %s was answered %s, so there was nothing to act on and nothing to record as applied", id, Get(entry, "disposition"))
	}
	before := Get(entry, "applied") == true
	if !before && Get(entry, "toldToAct") != true {
		return nil, ledgerError("no check of message %s has said act (it was held, for example while the relationship was paused), so there is nothing acted on to record; check it again and record it applied only after a check says act", id)
	}
	Set(&entry, "applied", true)
	Set(&answered, id, entry)
	Set(ledger, "answered", answered)
	return O("messageId", id, "contentDigest", Get(repeat, "contentDigest"), "applied", true, "alreadyApplied", before), nil
}
func SaveLedger(path string, ledger Obj) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer func() {
		if _, e := os.Stat(temporary); e == nil {
			err = errors.Join(err, os.Remove(temporary))
		}
	}()
	var indented bytes.Buffer
	if err = json.Indent(&indented, []byte(pyjson.Dumps(ledger, pyjson.Options{Compact: true, SortKeys: true})), "", "  "); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err = f.Write(indented.Bytes()); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func Observation(document any) (Obj, error) {
	o, ok := evidence.Object(document)
	if !ok {
		return nil, malformed("an observation is an object naming its source, not a %s", evidence.TypeName(document))
	}
	source := Get(o, "source")
	if evidence.TypeName(source) != "str" || strings.TrimSpace(str(source)) == "" {
		return nil, malformed("an observation names its source; a head with nowhere it was read is a value copied from somewhere, which is what this reading refuses to take")
	}
	unknown := []string{}
	for _, f := range o {
		if !slices.Contains([]string{"repository", "prNumber", "headSha", "artifactPath", "artifactDigest", "source", "observedAt"}, f.Key) {
			unknown = append(unknown, f.Key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, malformed("an observation carries only artifactDigest, artifactPath, headSha, prNumber, repository; it cannot answer %s", strings.Join(unknown, ", "))
	}
	for _, k := range []string{"repository", "prNumber", "headSha", "artifactPath", "artifactDigest"} {
		v := Get(o, k)
		if v == nil {
			continue
		}
		wanted := "str"
		valid := evidence.TypeName(v) == "str" && present(v)
		if k == "prNumber" {
			wanted = "int"
			n, ok := evidence.PyInt(v)
			valid = ok && n > 0
		}
		if !valid {
			return nil, malformed("%s in an observation is a %s, not %s", k, wanted, evidence.Repr(v))
		}
	}
	if v := Get(o, "observedAt"); v != nil && evidence.TypeName(v) != "str" {
		return nil, malformed("observedAt in an observation is a timestamp string")
	}
	return slices.Clone(o), nil
}
