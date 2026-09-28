package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

func stringNumber(v any) string { return fmt.Sprint(v) }
func (s *Service) launchRecord() Object {
	absent := func(why any) Object { return obj("path", nil, "declaredAt", nil, "declaredBy", nil, "unreadable", why) }
	path := s.path("launch-policy.json")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, e := os.Lstat(path); e == nil {
				return absent("the launch declaration " + path + " is a link to nothing")
			}
			return absent(nil)
		}
		return absent("the launch declaration could not be read: " + err.Error())
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return absent("the launch declaration could not be read: " + err.Error())
	}
	if !info.Mode().IsRegular() {
		return absent("the launch declaration " + path + " is not a regular file")
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return absent("the launch declaration could not be read: " + err.Error())
	}
	if detail := store.PythonJSONError(string(raw)); detail != "" {
		return absent("the launch declaration is not readable JSON: " + detail)
	}
	r, err := parse(raw)
	if err != nil {
		// Valid JSON of a non-object type declares no file, not corrupt JSON.
		return absent("the launch declaration names no execution policy file")
	}
	if strings.TrimSpace(text(get(r, "path"))) == "" {
		return absent("the launch declaration names no execution policy file")
	}
	return obj("path", get(r, "path"), "declaredAt", get(r, "declaredAt"), "declaredBy", get(r, "declaredBy"), "unreadable", nil)
}
func (s *Service) ResolveLaunchPolicy() Object {
	stated := strings.TrimSpace(os.Getenv(execution.EnvPolicy))
	record := s.launchRecord()
	declared := text(get(record, "path"))
	answer := obj("variable", execution.EnvPolicy, "path", nil, "source", nil, "record", nullable(declared), "environment", nullable(stated), "declaredAt", get(record, "declaredAt"), "declaredBy", get(record, "declaredBy"), "state", nil, "digest", nil, "persisted", nil, "detail", nil, "hint", nil)
	if truth(get(record, "unreadable")) {
		return set(answer, "source", "unreadable_record", "detail", get(record, "unreadable"), "hint", "declare the file again with service declare --execution-policy, or drop the record with service declare --forget-execution-policy. A launch does not fall back to this process's environment to cover an unreadable record")
	}
	canonical := func(path string) string {
		r, err := store.CanonicalSocket(path)
		if err != nil {
			return path
		}
		return r
	}
	if declared != "" && stated != "" && canonical(declared) != canonical(stated) {
		return set(answer, "source", "conflict", "detail", "this service declares "+store.PythonRepr(declared)+" and "+execution.EnvPolicy+" in this process names "+store.PythonRepr(stated), "hint", "two files are not a preference: unset the variable to launch on the declaration, or declare that other file")
	}
	path := declared
	if declared != "" {
		answer = set(answer, "path", declared, "source", "record", "persisted", true)
	} else if stated != "" {
		path = stated
		answer = set(answer, "path", stated, "source", "environment", "persisted", false, "hint", "this launch takes the policy from this process's environment and nothing records it, so a restart typed anywhere else loses it. Record it with service declare --execution-policy")
	} else {
		return set(answer, "detail", "no execution policy is declared for this service and "+execution.EnvPolicy+" is not set in this process, so a daemon launched from here can read none and withholds every role-bound delivery")
	}
	p := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: path})
	if p.Declared {
		return set(answer, "state", "declared", "digest", p.Digest())
	}
	return set(answer, "state", "unreadable", "detail", p.Detail)
}
func LaunchRefusal(r Object) Object {
	source := text(get(r, "source"))
	if source != "conflict" && source != "unreadable_record" {
		return nil
	}
	reason := "launch_policy_conflict"
	if source == "unreadable_record" {
		reason = "launch_policy_unreadable"
	}
	return obj("ok", false, "reason", reason, "detail", get(r, "detail"), "launchPolicy", r)
}
func (s *Service) Declare(path, actor string, forget bool) (out Object, err error) {
	if !forget {
		path, err = store.ExpandUser(path)
		if err != nil {
			return nil, err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		p := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: path})
		if !p.Declared {
			return obj("ok", false, "reason", "execution_policy_unreadable", "path", path, "detail", p.Detail, "launchPolicy", s.ResolveLaunchPolicy()), nil
		}
	}
	f, err := lockIfFree(s.path("daemon.lock"))
	if err != nil {
		return nil, err
	}
	if f != nil {
		defer func() { err = errors.Join(err, f.Close()) }()
	} else if r := s.holderRefusal(); r != nil {
		return set(r, "launchPolicy", s.ResolveLaunchPolicy(), "note", "the launch declaration is shared with the owner of this state directory and was left unchanged"), nil
	}
	note := "a running daemon keeps the policy it was launched with until it is restarted"
	if forget {
		before := s.launchRecord()
		if err = os.Remove(s.path("launch-policy.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return obj("ok", true, "reason", nil, "forgot", get(before, "path"), "actor", actor, "launchPolicy", s.ResolveLaunchPolicy(), "note", note), nil
	}
	written := obj("schemaVersion", 1, "path", path, "declaredAt", stamp(), "declaredBy", actor)
	if err = atomicWrite(s.path("launch-policy.json"), written); err != nil {
		return nil, err
	}
	return obj("ok", true, "reason", nil, "declared", written, "launchPolicy", s.ResolveLaunchPolicy(), "note", note), nil
}
