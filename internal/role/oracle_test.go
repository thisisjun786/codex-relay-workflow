package role

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/oracle-store.json holds what the CXC v0.2.40 oracle's store.ts answered (dist/store.js, global scope, CODEXCLAW_HOME in a
// temporary directory) over the cases of testdata/record-oracle.mjs, recorded once under Node 24 (no Node runs here). Every case is
// replayed through the Go API and must agree on each result, error and the store's bytes. By design the five cases that end in an
// error the oracle took from V8 or libuv compare the "cannot update subagent config: " prefix only (I7), and extras_number_forms
// compares the file with the opaque members kept as written (I6).

type recordedOp struct {
	Op     string          `json:"op"`
	Role   RoleName        `json:"role"`
	Patch  json.RawMessage `json:"patch"`
	Result *string         `json:"result"`
	Error  *string         `json:"error"`
	File   *string         `json:"file"`
}

type recordedCase struct {
	ID    string `json:"id"`
	Given struct {
		Store    *string `json:"store"`
		DirStore bool    `json:"dirStore"`
	} `json:"given"`
	Ops []recordedOp `json:"ops"`
}

func sameError(id, want, got string) bool {
	switch id {
	case "read_not_json", "read_empty_file", "read_trailing_garbage", "read_bom", "store_is_directory":
		const prefix = "cannot update subagent config: "
		return strings.HasPrefix(want, prefix) && strings.HasPrefix(got, prefix)
	}
	return want == got
}

// oracleFile is the file the oracle printed, with the opaque members of extras_number_forms as the Go port keeps them.
func oracleFile(id, file string) string {
	if id != "extras_number_forms" {
		return file
	}
	return strings.NewReplacer(`"n": 1,`, `"n": 1.0,`, "12345678901234567000", "12345678901234567890", `"neg0": 0`, `"neg0": -0`,
		`"e": 100`, `"e": 1e2`, "\"s\": \"A/\u2028\"", "\"s\": \"\\u0041\\/\u2028\"").Replace(file)
}

func TestOracleReplay(t *testing.T) {
	var cases []recordedCase
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-store.json"))
	check(t, err)
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("testdata: %v (%d cases)", err, len(cases))
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			env, dir := home(t)
			path := filepath.Join(dir, StoreFile)
			switch {
			case c.Given.DirStore:
				check(t, os.MkdirAll(path, 0o755))
			case c.Given.Store != nil:
				writeStore(t, dir, *c.Given.Store)
			}
			if c.Given.DirStore || c.Given.Store != nil {
				check(t, os.Chmod(dir, 0o755))
			}
			for i, op := range c.Ops {
				at := fmt.Sprintf("op %d (%s %s)", i, op.Op, op.Role)
				var got any
				var err error
				switch op.Op {
				case "settings":
					got, err = ReadSettings(env)
				case "config":
					got, err = ReadConfig(env)
				case "set":
					var patch RolePatch
					check(t, json.Unmarshal(op.Patch, &patch))
					got, err = SetRole(env, op.Role, patch)
				case "reset":
					got, err = ResetRole(env, op.Role)
				default:
					t.Fatalf("%s: unknown op", at)
				}
				switch {
				case op.Error != nil && err == nil:
					t.Fatalf("%s: no error, want %q", at, *op.Error)
				case op.Error != nil && !sameError(c.ID, *op.Error, err.Error()):
					t.Fatalf("%s: error %q, want %q", at, err, *op.Error)
				case op.Error == nil && err != nil:
					t.Fatalf("%s: error %v", at, err)
				case op.Error == nil:
					if text := must(Stringify(got, "")); string(text) != *op.Result {
						t.Fatalf("%s: result\n%s\nwant\n%s", at, text, *op.Result)
					}
				}
				data, readErr := os.ReadFile(path)
				if (readErr == nil) != (op.File != nil) {
					t.Fatalf("%s: store present = %v, want %v", at, readErr == nil, op.File != nil)
				}
				if op.File == nil {
					continue
				}
				if want := oracleFile(c.ID, *op.File); string(data) != want {
					t.Fatalf("%s: store\n%s\nwant\n%s", at, data, want)
				}
			}
		})
	}
}
