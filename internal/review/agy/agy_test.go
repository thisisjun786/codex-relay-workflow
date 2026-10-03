package agy

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCallShape(t *testing.T) {
	prompt := []byte("héllo 😀 review this") // more bytes than runes: agy counts bytes
	schema := []byte(`{"type":"object"}`)
	cfg, rec := fakeCfg(t, fakeSpec{Stdout: testdata(t, "success_schema.json")})
	res := run(t, cfg, Request{Prompt: prompt, Schema: schema})
	if res.Class != ClassNormal {
		t.Fatalf("class %s (%s): %s", res.Class, res.Reason, res.Detail)
	}
	got := rec()
	if !bytes.Equal(got.Stdin, prompt) || !got.StdinIsPipe {
		t.Errorf("the prompt must arrive on a stdin pipe unchanged: %q pipe=%v", got.Stdin, got.StdinIsPipe)
	}
	for _, bad := range []string{"--print=-", "--print", "-p", "--print-timeout", "--dangerously-skip-permissions", "--mode", "--sandbox"} {
		if slices.Contains(got.Args, bad) {
			t.Errorf("argument %q must never be passed: %v", bad, got.Args)
		}
	}
	for _, fd := range got.Fds {
		if filepath.Base(fd) == "agy.lock" {
			t.Errorf("agy inherited the host-wide lock: %v", got.Fds)
		}
	}
	if len(got.Args) != 9 {
		t.Fatalf("arguments: %v", got.Args)
	}
	schemaPath, logPath := got.Args[5], got.Args[8]
	got.Args[5], got.Args[8] = "<schema>", "<log>"
	want := []string{"--model", "gemini-3.8-flash-high", "--output-format", "json", "--json-schema", "<schema>", "--disable-slash-commands", "--log-file", "<log>"}
	if !slices.Equal(got.Args, want) {
		t.Errorf("arguments %v, want %v", got.Args, want)
	}
	if got.Schema != string(schema) || !strings.HasPrefix(schemaPath, cfg.WorkRoot) || !strings.HasPrefix(logPath, cfg.WorkRoot) {
		t.Errorf("schema %q at %s, log at %s", got.Schema, schemaPath, logPath)
	}
	if res.Model != "Gemini 3.8 Flash (High)" || res.Usage.TotalTokens != 7949 || res.ExitCode != 0 || res.Elapsed <= 0 {
		t.Errorf("served model %q, usage %+v, exit %d, elapsed %s", res.Model, res.Usage, res.ExitCode, res.Elapsed)
	}
	if !bytes.Contains(res.StructuredOutput, []byte(`"line":8`)) || res.Reason != "" {
		t.Errorf("structured output %s, reason %q", res.StructuredOutput, res.Reason)
	}
}

// TestModelComesFromTheConfig: the model on the command line is the config value; the Result names the model only if agy's log does, never the one asked for.
func TestModelComesFromTheConfig(t *testing.T) {
	cfg, rec := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`, NoLabel: true})
	cfg.Model = "model-from-config"
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	args := rec().Args
	if res.Class != ClassNormal || res.Model != "" {
		t.Errorf("%s/%s, served model %q: with no label in the log it is unknown", res.Class, res.Reason, res.Model)
	}
	if args[0] != "--model" || args[1] != "model-from-config" || slices.Contains(args, "--json-schema") || len(args) != 7 {
		t.Errorf("arguments %v: the model is the config value and no schema means no --json-schema", args)
	}
}

func TestEnvironmentIsScrubbed(t *testing.T) {
	for k, v := range map[string]string{"GH_TOKEN": "t", "GITHUB_TOKEN": "t", "SSH_AUTH_SOCK": "/sock", "AWS_SECRET_ACCESS_KEY": "t", "ANTHROPIC_API_KEY": "t",
		"MY_SERVICE_PASSWORD": "t", "XDG_SESSION_TOKEN": "t", "HOME": "/home/test", "XDG_CONFIG_HOME": "/xdg/config", "XDG_DATA_HOME": "/xdg/data", "LC_ALL": "C"} {
		t.Setenv(k, v)
	}
	cfg, rec := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`})
	run(t, cfg, Request{Prompt: []byte("hi")})
	seen := map[string]string{}
	for _, kv := range rec().Env {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY", "MY_SERVICE_PASSWORD", "XDG_SESSION_TOKEN"} {
		if _, ok := seen[k]; ok {
			t.Errorf("%s reached agy", k)
		}
	}
	for k, v := range map[string]string{"HOME": "/home/test", "XDG_CONFIG_HOME": "/xdg/config", "XDG_DATA_HOME": "/xdg/data", "LC_ALL": "C", "PATH": os.Getenv("PATH")} {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q: agy finds its login through it", k, seen[k], v)
		}
	}
}

func TestScrubEnvAllowlist(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/h", "XDG_STATE_HOME=/s", "XDG_SESSION_TOKEN=t", "GH_TOKEN=t", "TOKEN=t", "LANG=C", "LC_ALL=C", "LC_SECRET=t", "https_proxy=p", "PATHX=1"}
	want := []string{"PATH=/bin", "HOME=/h", "XDG_STATE_HOME=/s", "LANG=C", "LC_ALL=C", "https_proxy=p"}
	if got := scrubEnv(environ); !slices.Equal(got, want) {
		t.Errorf("scrubEnv = %v, want %v", got, want)
	}
}

func TestNotStartedAndEmptyPrompt(t *testing.T) {
	cfg, _ := fakeCfg(t, fakeSpec{})
	cfg.Binary = filepath.Join(t.TempDir(), "no-such-agy")
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	if res.Class != ClassUnavailable || res.Reason != ReasonNotStarted || res.ExitCode != -1 {
		t.Errorf("a missing binary: %s/%s exit %d", res.Class, res.Reason, res.ExitCode)
	}
	if _, err := Run(t.Context(), cfg, Request{}); err == nil {
		t.Error("an empty prompt must be refused")
	}
}
