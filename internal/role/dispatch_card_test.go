package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func TestDispatchCardOracleGoldens(t *testing.T) {
	for _, name := range []string{"missing", "verified", "filtered"} {
		for _, v1 := range []bool{false, true} {
			file := "card-" + name
			if v1 {
				file += "-v1"
			}
			t.Run(file, func(t *testing.T) {
				m, env := fallbackTestEnv(t)
				models := []any{}
				if name == "verified" {
					for _, id := range ModelAliases() {
						models = append(models, id)
					}
				}
				if name == "filtered" {
					models = []any{map[string]any{"id": "command-code/deepseek-deepseek-v4.1-flash"}, map[string]any{"id": "devin/swe-2", "disabled": true}, map[string]any{"id": "kimi/kimi-for-coding-highspeed", "visibility": "hide"}}
				}
				if name != "missing" {
					b, _ := json.Marshal(map[string]any{"models": models})
					if err := os.WriteFile(m["CODEX_MODELS_CACHE_PATH"], b, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if v1 {
					m["CRW_SPAWN_V1"] = "1"
				}
				got, err := RenderDispatchCard(env)
				if err != nil {
					t.Fatal(err)
				}
				want := string(fallbackFixture(t, file+".golden", nil))
				if got != want {
					t.Fatalf("card differs from recorded oracle:\n%s", got)
				}
				if len(utf16.Encode([]rune(got))) > 1200 {
					t.Fatal("card over budget")
				}
			})
		}
	}
}

func TestDispatchAliasOracleMembershipAndPassthrough(t *testing.T) {
	m, env := fallbackTestEnv(t)
	b := []byte(`{"models":[{"id":"command-code/deepseek-deepseek-v4.1-flash"},{"id":"devin/swe-2","disabled":true},{"id":"kimi/kimi-for-coding-highspeed","visibility":"hide"}]}`)
	if err := os.WriteFile(m["CODEX_MODELS_CACHE_PATH"], b, 0600); err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input string
		Want  ResolvedAlias
	}
	fallbackFixture(t, "aliases.json", &cases)
	for _, c := range cases {
		if got := ResolveDispatchAlias(c.Input, env); got != c.Want {
			t.Errorf("%s: %+v want %+v", c.Input, got, c.Want)
		}
	}
	if AliasMapDate != "2026-09-24" {
		t.Fatal("map date")
	}
	first := ModelAliases()
	delete(first, "deepseek")
	if len(ModelAliases()) != 5 {
		t.Fatal("alias table shared mutable state")
	}
	if err := os.Remove(m["CODEX_MODELS_CACHE_PATH"]); err != nil {
		t.Fatal(err)
	}
	if got := ResolveDispatchAlias("deepseek", env); got.Verified {
		t.Fatal("missing catalog verified")
	}
	if _, err := os.Stat(filepath.Join(m["CRW_HOME"], StoreFile)); !os.IsNotExist(err) {
		t.Fatal("card wrote settings")
	}
}
