package sync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func packetPolicy(state, environment string) (registry.RolePolicy, error) {
	stated := strings.TrimSpace(environment)
	path := filepath.Join(state, "launch-policy.json")
	declared := ""
	var at, by any
	unreadable := ""
	file, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(e, os.ErrNotExist) {
		if _, e := os.Lstat(path); e == nil {
			unreadable = "the launch declaration " + path + " is a link to nothing"
		}
	} else if e != nil {
		unreadable = "the launch declaration could not be read: " + store.PythonOSErrorText(e)
	} else {
		info, e := file.Stat()
		if e != nil {
			unreadable = "the launch declaration could not be read: " + store.PythonOSErrorText(e)
		} else if !info.Mode().IsRegular() {
			unreadable = "the launch declaration " + path + " is not a regular file"
		} else {
			raw, e := io.ReadAll(file)
			if e != nil {
				unreadable = "the launch declaration could not be read: " + store.PythonOSErrorText(e)
			} else if !utf8.Valid(raw) {
				for i, b := range raw {
					if b >= 0x80 {
						unreadable = fmt.Sprintf("the launch declaration is not readable JSON: 'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", b, i)
						break
					}
				}
			} else if problem := reception.JSONDepthProblem(raw); problem != "" {
				unreadable = "the launch declaration is not readable JSON: " + problem
			} else {
				v, e := registry.DecodeJSON(string(raw))
				if e != nil {
					unreadable = "the launch declaration is not readable JSON: " + e.Error()
				} else {
					declared = text(reception.Get(v, "path"))
					if strings.TrimSpace(declared) == "" {
						unreadable = "the launch declaration names no execution policy file"
					} else {
						at, by = reception.Get(v, "declaredAt"), reception.Get(v, "declaredBy")
					}
				}
			}
		}
		if e = file.Close(); e != nil {
			return registry.RolePolicy{}, e
		}
	}
	source, detail, hint, reason := "", "", "", ""
	canonical := func(p string) string {
		v, e := store.ResolvePath(p)
		if e != nil {
			return p
		}
		return v
	}
	if unreadable != "" {
		source, detail, reason = "unreadable_record", unreadable, "launch_policy_unreadable"
		hint = "declare the file again with service declare --execution-policy, or drop the record with service declare --forget-execution-policy. A launch does not fall back to this process's environment to cover an unreadable record"
	} else if declared != "" && stated != "" && canonical(declared) != canonical(stated) {
		source, reason = "conflict", "launch_policy_conflict"
		detail = fmt.Sprintf("this service declares %s and %s in this process names %s", store.PyRepr(declared), execution.EnvPolicy, store.PyRepr(stated))
		hint = "two files are not a preference: unset the variable to launch on the declaration, or declare that other file"
	}
	if reason != "" {
		nullable := func(s string) any {
			if s == "" {
				return nil
			}
			return s
		}
		resolution := obj("variable", execution.EnvPolicy, "path", nil, "source", source, "record", nullable(declared), "environment", nullable(stated), "declaredAt", at, "declaredBy", by, "state", nil, "digest", nil, "persisted", nil, "detail", detail, "hint", hint)
		return registry.RolePolicy{}, &cli.PayloadExit{Payload: obj("ok", false, "reason", reason, "detail", detail, "launchPolicy", resolution), Code: 2}
	}
	if declared != "" {
		stated = declared
	}
	policy := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: stated})
	if stated != "" {
		if raw, e := os.ReadFile(stated); e == nil {
			if problem := reception.JSONDepthProblem(raw); problem != "" {
				policy = registry.RolePolicy{Detail: stated + " is nested deeper than the execution policy parser can read", PublicDetail: "configured execution policy is unreadable or invalid"}
			}
		}
	}
	return policy, nil
}
