package crwconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func integrationConfig(t *testing.T, body string) (IntegrationSettings, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return LoadIntegration(func(name string) string {
		if name == "CRW_CONFIG" {
			return path
		}
		return ""
	}, "")
}

func TestRequiredStatusConfig(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"absent", `{"manage":{}}`, "", false},
		{"empty", `{"integration":{"required_status":""}}`, "", false},
		{"custom", `{"integration":{"required_status":"dev-gate"}}`, "dev-gate", false},
		{"extra field", `{"integration":{"required_status":"dev-gate","other":1}}`, "dev-gate", false},
		{"section null", `{"integration":null}`, "", true},
		{"section list", `{"integration":["dev-gate"]}`, "", true},
		{"field null", `{"integration":{"required_status":null}}`, "", true},
		{"field number", `{"integration":{"required_status":3}}`, "", true},
		{"whitespace", `{"integration":{"required_status":" dev-gate"}}`, "", true},
		{"control", `{"integration":{"required_status":"dev\u0007gate"}}`, "", true},
		{"tab", `{"integration":{"required_status":"dev\tgate"}}`, "", true},
	}
	for _, c := range cases {
		got, err := integrationConfig(t, c.body)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: err = nil, want a refusal", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if got.RequiredStatus != c.want {
			t.Errorf("%s: RequiredStatus = %q, want %q", c.name, got.RequiredStatus, c.want)
		}
	}
}

func TestRequiredStatusConfigUnreadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadIntegration(func(name string) string {
		if name == "CRW_CONFIG" {
			return dir
		}
		return ""
	}, ""); err == nil {
		t.Error("a configured path that is a directory loaded")
	}
}
