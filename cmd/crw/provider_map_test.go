package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func portCommand(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProviderMapCommandIngress(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CRW_HOME", root)
	t.Setenv("PATH", root)
	t.Setenv("CRW_MAP_BOOTSTRAP", "")
	t.Setenv("CRW_PYTHON", "")
	portCommand(t, root, "ocx", `test "$*" = 'status --json' || exit 9
printf '%s\n' '{"proxy":{"running":true},"defaultProvider":"openai","listen":{"port":10100}}'`)
	portCommand(t, root, "uv", "exit 1")
	portCommand(t, root, "python3", `test "$1" = '-B' || exit 9
printf '%s\n' 'fake map'`)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"provider", "detect"}, `"mode":"provider"`},
		{[]string{"provider", "--help", "ignored"}, `"port":10100`},
		{[]string{"map", ".", "--tokens", "20"}, "fake map\n"},
		{[]string{"map"}, "fake map\n"},
	} {
		var out, stderr strings.Builder
		code := run(context.Background(), "crw", c.args, &out, &stderr)
		if code != 0 || !strings.Contains(out.String(), c.want) || stderr.Len() != 0 {
			t.Errorf("%v: exit %d stdout %q stderr %q", c.args, code, out.String(), stderr.String())
		}
	}
	for _, arg := range []string{"--leg", "--leg="} {
		args := []string{"session-start", arg + "session-start-ensuring-provider-bridge"}
		if arg == "--leg" {
			args = []string{"session-start", arg, "session-start-ensuring-provider-bridge"}
		}
		var out, stderr strings.Builder
		claimed, code := runComponentHook(invocation{ctx: context.Background(), args: args, stdout: &out, stderr: &stderr}, strings.NewReader("not JSON"), componentHooks())
		if !claimed || code != 0 || !strings.Contains(out.String(), `"hookEventName":"SessionStart"`) || !strings.Contains(out.String(), `\"mode\":\"provider\"`) {
			t.Errorf("hook %v: claimed %v exit %d output %q", args, claimed, code, out.String())
		}
	}
}
