package integrator

import (
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Checkout: "/c", QueueDir: "/q", LockFile: "/l", RunsDir: "/r", WorkRoot: "/w",
		GateCommand: []string{"gate"}, VerifyCommand: []string{"verify"}, RecordVersionCommand: []string{"record"},
	}
}

func TestConfigResolvedFillsOnlyNonHostDefaults(t *testing.T) {
	got, err := validConfig().Resolved()
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote != "origin" || got.Branch != "dev" || got.Git != "git" || got.KeyPrefix != "CRW" || got.TagPrefix != "cc-q" ||
		got.PluginDir != "plugins/crw" || got.PluginManifest != "plugins/crw/.codex-plugin/plugin.json" || got.IntervalSeconds != 60 {
		t.Fatalf("%+v", got)
	}
	if got.remoteRef() != "refs/remotes/origin/dev" || got.key("12") != "CRW-12" || got.node("12") != "crw-12" {
		t.Fatalf("%s %s %s", got.remoteRef(), got.key("12"), got.node("12"))
	}
}

func TestConfigEveryHostPathAndCommandIsRequired(t *testing.T) {
	for _, field := range []string{"checkout", "queue_dir", "lock_file", "runs_dir", "work_root", "gate_command", "verify_command", "record_version_command"} {
		c := validConfig()
		switch field {
		case "checkout":
			c.Checkout = ""
		case "queue_dir":
			c.QueueDir = ""
		case "lock_file":
			c.LockFile = ""
		case "runs_dir":
			c.RunsDir = ""
		case "work_root":
			c.WorkRoot = ""
		case "gate_command":
			c.GateCommand = nil
		case "verify_command":
			c.VerifyCommand = []string{""}
		case "record_version_command":
			c.RecordVersionCommand = nil
		}
		if _, err := c.Resolved(); err == nil || !strings.Contains(err.Error(), field+" is required") {
			t.Errorf("%s: %v", field, err)
		}
	}
	if _, err := (Config{}).Resolved(); err == nil {
		t.Fatal("an empty section is accepted")
	}
}

func TestConfigRefusesWhatCouldEscapeOrInject(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"relative checkout":     func(c *Config) { c.Checkout = "repo" },
		"relative work root":    func(c *Config) { c.WorkRoot = "work" },
		"remote with a slash":   func(c *Config) { c.Remote = "a/b" },
		"remote as an option":   func(c *Config) { c.Remote = "-x" },
		"branch as an option":   func(c *Config) { c.Branch = "--force" },
		"branch with dots":      func(c *Config) { c.Branch = "a..b" },
		"branch with a space":   func(c *Config) { c.Branch = "a b" },
		"key prefix with dash":  func(c *Config) { c.KeyPrefix = "A-B" },
		"tag prefix with slash": func(c *Config) { c.TagPrefix = "a/b" },
		"absolute plugin dir":   func(c *Config) { c.PluginDir = "/plugins" },
		"escaping manifest":     func(c *Config) { c.PluginManifest = "../x.json" },
		"unclean manifest":      func(c *Config) { c.PluginManifest = "a/../b.json" },
		"negative interval":     func(c *Config) { c.IntervalSeconds = -1 },
	} {
		c := validConfig()
		mutate(&c)
		if _, err := c.Resolved(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
