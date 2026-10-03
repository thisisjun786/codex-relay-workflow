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

func TestModelComesFromTheConfig(t *testing.T) {
	cfg, rec := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`})
	cfg.Model = "model-from-config"
	if res := run(t, cfg, Request{Prompt: []byte("hi")}); res.Class != ClassNormal {
		t.Fatalf("class %s (%s): %s", res.Class, res.Reason, res.Detail)
	}
	args := rec().Args
	if args[0] != "--model" || args[1] != "model-from-config" || slices.Contains(args, "--json-schema") || len(args) != 7 {
		t.Errorf("arguments %v: the model is the config value and no schema means no --json-schema", args)
	}
}

func TestEnvironmentIsScrubbed(t *testing.T) {
	for k, v := range map[string]string{"GH_TOKEN": "t", "GITHUB_TOKEN": "t", "SSH_AUTH_SOCK": "/sock", "AWS_SECRET_ACCESS_KEY": "t", "ANTHROPIC_API_KEY": "t",
		"MY_SERVICE_PASSWORD": "t", "HOME": "/home/test", "XDG_CONFIG_HOME": "/xdg/config", "XDG_DATA_HOME": "/xdg/data", "LC_ALL": "C"} {
		t.Setenv(k, v)
	}
	cfg, rec := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`})
	run(t, cfg, Request{Prompt: []byte("hi")})
	seen := map[string]string{}
	for _, kv := range rec().Env {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY", "MY_SERVICE_PASSWORD"} {
		if _, ok := seen[k]; ok {
			t.Errorf("%s reached agy", k)
		}
	}
	for k, v := range map[string]string{"HOME": "/home/test", "XDG_CONFIG_HOME": "/xdg/config", "XDG_DATA_HOME": "/xdg/data", "LC_ALL": "C", "PATH": os.Getenv("PATH")} {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q: agy finds its login through it", k, seen[k], v)
		}
	}
	if seen["CRW_FAKE_AGY"] == "" {
		t.Error("a variable from Config.Env must reach agy")
	}
}

func TestScrubEnvAllowlist(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/h", "XDG_STATE_HOME=/s", "GH_TOKEN=t", "TOKEN=t", "LANG=C", "https_proxy=p", "PATHX=1", "FOO=old"}
	got := scrubEnv(environ, []string{"FOO=new"})
	want := []string{"PATH=/bin", "HOME=/h", "XDG_STATE_HOME=/s", "LANG=C", "https_proxy=p", "FOO=new"}
	if !slices.Equal(got, want) {
		t.Errorf("scrubEnv = %v, want %v", got, want)
	}
}

func TestDefaultsAndNotStarted(t *testing.T) {
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
