package hook

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func Test33NativeRegistrationPATH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	native := filepath.Join(bin, "crw")
	// Status must not execute a native hook as a Python interpreter.
	writeTest(t, native, []byte("#!/bin/sh\nexit 91\n"))
	if err := os.Chmod(native, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, command, path, want string }{
		{"absolute", native + " hook", "", present},
		{"home", `"$HOME/bin/crw" hook; exit 0`, "", present},
		{"braced_home", `"${HOME}/bin/crw" hook`, "", present},
		{"path_found", "crw hook", bin, present},
		{"path_missing", "crw hook", filepath.Join(home, "missing"), absent},
		{"relative_path", "./bin/crw hook", bin, relativeAdapter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			writeStatusJSON(t, filepath.Join(home, "hooks.json"), map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": tc.command}}}}}})
			status := Status(context.Background(), home, nil, "Stop")
			for _, name := range []string{"registeredCommandTarget", "registeredInterpreter"} {
				cell := status[name].(map[string]any)
				if cell["value"] != tc.want {
					t.Fatalf("%s: %v", name, cell)
				}
			}
			target := status["registeredCommandTarget"].(map[string]any)
			if tc.want == present && target["path"] != native {
				t.Fatal(target)
			}
		})
	}
}

func Test33PythonBareLauncherUnchanged(t *testing.T) {
	home := t.TempDir()
	launcher := filepath.Join(home, "python-launcher")
	if err := os.Symlink(python(t), launcher); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home)
	entry := filepath.Join(home, "completion_hook.py")
	writeTest(t, entry, []byte("# adapter\n"))
	command := "python-launcher " + entry
	writeStatusJSON(t, filepath.Join(home, "hooks.json"), map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": command}}}}}})
	_, registrations, _ := readRegistrations(filepath.Join(home, "hooks.json"), "Stop")
	target, interp, _ := probeRegistrations(context.Background(), registrations)
	if len(registrations) != 1 || registrations[0].Native || target["value"] != present || interp["value"] != present {
		t.Fatal(registrations, target, interp)
	}
	cmd := exec.Command(python(t), "-c", `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;c=sys.argv[2];print(json.dumps(completion._interpreter_cell([{'identity':'test','command':c,'target':sys.argv[3]}])))`, filepath.Join(testRoot, "scripts"), command, entry)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	var expected map[string]any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	if expected["value"] != interp["value"] {
		t.Fatal(expected, interp)
	}
	pyProbe := expected["probes"].([]any)[0].(map[string]any)
	goProbe := interp["probes"].([]any)[0].(map[string]any)
	if pyProbe["resolved"] != goProbe["resolved"] || goProbe["resolved"] != launcher {
		t.Fatal(pyProbe, goProbe)
	}
}
