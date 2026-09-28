// Package service owns daemon scope, process identity, launch intent and supervision.
package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Object = contract.OrderedObject

func obj(values ...any) Object {
	out := Object{}
	for i := 0; i < len(values); i += 2 {
		out = append(out, contract.Field{Key: values[i].(string), Value: values[i+1]})
	}
	return out
}
func get(o Object, k string) any {
	for _, f := range o {
		if f.Key == k {
			return f.Value
		}
	}
	return nil
}
func text(v any) string { s, _ := v.(string); return s }
func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
func truth(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case json.Number:
		return x != "0"
	}
	return true
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func set(o Object, values ...any) Object {
	out := append(Object{}, o...)
	for i := 0; i < len(values); i += 2 {
		k := values[i].(string)
		found := false
		for j := range out {
			if out[j].Key == k {
				out[j].Value = values[i+1]
				found = true
				break
			}
		}
		if !found {
			out = append(out, contract.Field{Key: k, Value: values[i+1]})
		}
	}
	return out
}
func stamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return hex.EncodeToString(b[:]), nil
}
func decode(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			o := Object{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				v, e := decode(d)
				if e != nil {
					return nil, e
				}
				o = append(o, contract.Field{Key: k.(string), Value: v})
			}
			_, err = d.Token()
			return o, err
		case '[':
			a := []any{}
			for d.More() {
				v, e := decode(d)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, err = d.Token()
			return a, err
		}
	}
	return t, nil
}
func parse(raw []byte) (Object, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decode(d)
	if err != nil {
		return nil, err
	}
	o, ok := v.(Object)
	if !ok {
		return nil, fmt.Errorf("record is not an object")
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("record has trailing data")
	}
	return o, nil
}
func read(path string) Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	o, _ := parse(raw)
	return o
}
func encoded(o Object) ([]byte, error) {
	var b bytes.Buffer
	if err := contract.Emit(&b, o); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

// Python opens service records with the ordinary 0666 creation mode; the
// process umask decides the permissions, including for atomic replacements.
func temporaryRecord(path string) (*os.File, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
}
func atomicWrite(path string, o Object) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := encoded(o)
	if err != nil {
		return err
	}
	f, err := temporaryRecord(path)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

const ScopeEnv = "CODEX_SESSION_RELAY_SCOPE_DIR"
const SettledEnv = "CODEX_SESSION_RELAY_LAUNCH_POLICY_SETTLED"
const ExitBoundSpent = 5

type Refused struct{ Reason, Detail string }

func (e *Refused) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Reason
}

type Service struct {
	Selection                                 store.StateSelection
	Socket, StoreID, InstallationID, LaunchID string
	StoreUnidentified, Takeover               bool
	Scope                                     *ScopeRegistry
	LaunchEnvironment                         Object
	// Prepare opens the writable store only after daemon and scope exclusion.
	Prepare func() error
}

func InstallationID(state string) string {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		executable = ""
	}
	resolved, err := store.ResolvePath(state)
	if err != nil {
		resolved = state
	}
	sum := sha256.Sum256([]byte(filepath.Dir(executable) + "\x00" + resolved))
	return hex.EncodeToString(sum[:8])
}
func New(ctx context.Context, selection store.StateSelection, socket string) (*Service, error) {
	scope, err := ResolveScope()
	if err != nil {
		return nil, err
	}
	s := &Service{Selection: selection, Socket: socket, Scope: scope, InstallationID: InstallationID(selection.Path)}
	p := store.Probe(ctx, selection)
	s.StoreID = p.Store.StoreID
	s.StoreUnidentified = p.Access.DBExists && s.StoreID == ""
	return s, nil
}
func (s *Service) path(name string) string    { return filepath.Join(s.Selection.Path, name) }
func (s *Service) Record() Object             { return read(s.path("daemon.json")) }
func (s *Service) WriteRecord(o Object) error { return atomicWrite(s.path("daemon.json"), o) }
func (s *Service) note(values ...any) error {
	if r := s.Record(); r != nil {
		return s.WriteRecord(set(r, values...))
	}
	return nil
}
func (s *Service) NewRecord(pid int, token string) Object {
	return obj("pid", pid, "startTicks", StartTicks(pid), "bootId", BootID(), "workerPid", nil, "token", nullable(token), "launchId", nullable(s.LaunchID), "takeover", func() any {
		if s.Takeover {
			return true
		}
		return nil
	}(), "readyAt", nil, "storeId", nullable(s.StoreID), "installationId", s.InstallationID, "stateDir", s.Selection.Path, "socketPath", nullable(s.Socket), "scopeAuthority", s.Scope.Authority, "scopeRoot", s.Scope.Root, "startedAt", stamp(), "restarts", 0, "consecutiveFailures", 0, "lastExit", nil, "nextRestartAt", nil)
}
func (s *Service) JournalNote(detail string) error {
	// A pre-start spent bound has no state directory and Python leaves no log.
	if _, err := os.Stat(s.Selection.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path("daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s service_degraded: %s\n", stamp(), detail)
	return errors.Join(err, f.Close())
}
func (s *Service) Intent() Object {
	r := read(s.path("service.json"))
	return obj("enabled", truth(get(r, "enabled")), "configured", r != nil, "changedAt", get(r, "changedAt"), "changedBy", get(r, "changedBy"))
}
func (s *Service) writeIntent(enabled bool, actor string) (Object, error) {
	r := obj("enabled", enabled, "changedAt", stamp(), "changedBy", actor)
	if err := atomicWrite(s.path("service.json"), r); err != nil {
		return nil, err
	}
	return set(r, "configured", true), nil
}
func (s *Service) AuthorityCheck(allow bool) Object {
	if s.Scope.Authority == "isolated" && !allow {
		return obj("ok", false, "reason", "isolated_scope_not_allowed", "detail", ScopeEnv+" is set, which moves the ownership record away from the one authority every launch shares. Pass --allow-isolated-scope if that is deliberate; single ownership is not enforced across differently isolated roots.")
	}
	return obj("ok", true, "reason", nil)
}
func BootID() any {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil
	}
	return strings.TrimSpace(string(raw))
}
func statFields(pid int) []string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return nil
	}
	return strings.Fields(string(raw)[end+2:])
}
func StartTicks(pid int) any {
	fields := statFields(pid)
	if len(fields) < 20 {
		return nil
	}
	n, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return nil
	}
	return n
}
func ProcessState(pid int) string {
	f := statFields(pid)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}
