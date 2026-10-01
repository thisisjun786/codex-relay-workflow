package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func stringNumber(v any) string { return fmt.Sprint(v) }

// launchDepth is the container depth LaunchPolicy.read's json.loads decodes before the C
// scanner raises RecursionError (at 9999, measured against CPython 3.13 through the relay CLI).
const launchDepth = 9998

// launchRecord is LaunchPolicy.read for the declaration at path: the declaration, its absence,
// or why it cannot be read, in the fence's words. A path that is there and cannot be read as a
// regular file of UTF-8 JSON is unreadable, never absent.
func launchRecord(path string) Object {
	absent := func(why any) Object { return obj("path", nil, "declaredAt", nil, "declaredBy", nil, "unreadable", why) }
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, e := os.Lstat(path); e == nil {
				return absent("the launch declaration " + path + " is a link to nothing")
			}
			return absent(nil)
		}
		return absent("the launch declaration could not be read: " + store.PythonOSErrorText(err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return absent("the launch declaration could not be read: " + descriptorError(err))
	}
	if !info.Mode().IsRegular() {
		return absent("the launch declaration " + path + " is not a regular file")
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return absent("the launch declaration could not be read: " + descriptorError(err))
	}
	decoded, err := store.DecodeUTF8(raw)
	if err != nil {
		return absent("the launch declaration is not readable JSON: " + err.Error())
	}
	if message, _ := store.PythonJSONErrorWithLimit(decoded, launchDepth); message != "" {
		return absent("the launch declaration is not readable JSON: " + message)
	}
	data, err := store.LoadsJSON([]byte(decoded))
	if err != nil {
		return absent("the launch declaration is not readable JSON: " + err.Error())
	}
	// Valid JSON that is not an object declares no file; it is not corrupt JSON.
	record, _ := data.(Object)
	declared, ok := get(record, "path").(string)
	if !ok || pyvalue.Strip(declared) == "" {
		return absent("the launch declaration names no execution policy file")
	}
	return obj("path", declared, "declaredAt", get(record, "declaredAt"), "declaredBy", get(record, "declaredBy"), "unreadable", nil)
}

// descriptorError is str(OSError) for a failure on an open descriptor (fstat, read): Python
// names no file there.
func descriptorError(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return store.PythonOSErrorText(errno)
	}
	return err.Error()
}

// ResolveLaunchPolicy is RelayService.resolve_launch_policy over this process's environment.
func (s *Service) ResolveLaunchPolicy() Object {
	return ResolveLaunchPolicyAt(s.Selection.Path, os.Getenv(execution.EnvPolicy))
}

// ResolveLaunchPolicyAt is RelayService.resolve_launch_policy(environ) for the service of state
// directory state, environment being environ's CODEX_THREAD_BRIDGE_EXECUTION_POLICY ("" when
// unset), the one variable it reads. It is the only reader of launch-policy.json: service
// status, declare, start, restart and run, doctor and the store-backed packet-check all
// resolve through it.
func ResolveLaunchPolicyAt(state, environment string) Object {
	stated := pyvalue.Strip(environment)
	record := launchRecord(stateFile(state, "launch-policy.json"))
	declared := text(get(record, "path"))
	answer := obj("variable", execution.EnvPolicy, "path", nil, "source", nil, "record", nullable(declared), "environment", nullable(stated), "declaredAt", get(record, "declaredAt"), "declaredBy", get(record, "declaredBy"), "state", nil, "digest", nil, "persisted", nil, "detail", nil, "hint", nil)
	if truth(get(record, "unreadable")) {
		return set(answer, "source", "unreadable_record", "detail", get(record, "unreadable"), "hint", "declare the file again with service declare --execution-policy, or drop the record with service declare --forget-execution-policy. A launch does not fall back to this process's environment to cover an unreadable record")
	}
	if declared != "" && stated != "" && canonicalPolicyPath(declared) != canonicalPolicyPath(stated) {
		return set(answer, "source", "conflict", "detail", "this service declares "+pyvalue.StrRepr(declared)+" and "+execution.EnvPolicy+" in this process names "+pyvalue.StrRepr(stated), "hint", "two files are not a preference: unset the variable to launch on the declaration, or declare that other file")
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

// canonicalPolicyPath is canonical_policy_path: one spelling of a policy file for comparing two
// (canonical_socket's formula), or the value itself where the OS refuses the path.
func canonicalPolicyPath(value string) string {
	canonical, err := store.CanonicalSocket(value)
	if err != nil {
		return value
	}
	return canonical
}

// ErrEmbeddedNUL is os.environ's refusal of a value holding NUL, Python's
// ValueError("embedded null byte").
var ErrEmbeddedNUL = errors.New("embedded null byte")

// LaunchVariable is the value _apply_launch_policy puts into this process's
// CODEX_THREAD_BRIDGE_EXECUTION_POLICY once resolution is not refused: the recorded path when
// the record is its source (set true); otherwise the environment stands and set is false. A
// recorded path holding NUL is ErrEmbeddedNUL, the host failure the assignment raises.
func LaunchVariable(resolution Object) (value string, set bool, err error) {
	if get(resolution, "source") != "record" {
		return "", false, nil
	}
	value = text(get(resolution, "path"))
	if strings.IndexByte(value, 0) >= 0 {
		return "", false, ErrEmbeddedNUL
	}
	return value, true, nil
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

// errNoHome is pathlib's RuntimeError for an unknown ~user, worded as Python words it.
//
//lint:ignore ST1005 pathlib's caller-visible message kept byte-identical to Python
var errNoHome = errors.New("Could not determine home directory.")

func (s *Service) Declare(path, actor string, forget bool) (out Object, err error) {
	if !forget {
		// Path(path).expanduser().absolute(): expanded and absolute but neither resolved nor
		// folded, so the file recorded is the file just checked, '..' after a symlink included.
		path, err = store.AbsoluteExpanded(path)
		if errors.Is(err, store.ErrNoHome) {
			return nil, errNoHome
		}
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
		before := launchRecord(s.path("launch-policy.json"))
		// Path.unlink(missing_ok=True): unlink(2) alone, so a directory in its place is
		// IsADirectoryError rather than removed.
		if e := syscall.Unlink(s.path("launch-policy.json")); e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, &os.PathError{Op: "unlink", Path: s.path("launch-policy.json"), Err: e}
		}
		return obj("ok", true, "reason", nil, "forgot", get(before, "path"), "actor", actor, "launchPolicy", s.ResolveLaunchPolicy(), "note", note), nil
	}
	written := obj("schemaVersion", 1, "path", path, "declaredAt", stamp(), "declaredBy", actor)
	if err = atomicWrite(s.path("launch-policy.json"), written); err != nil {
		return nil, err
	}
	return obj("ok", true, "reason", nil, "declared", written, "launchPolicy", s.ResolveLaunchPolicy(), "note", note), nil
}
