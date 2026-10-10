package role

import (
	"encoding/json"
	"errors"
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

// oracleUnusableReads are the recorded cases whose store, or one of whose roles, the oracle read as no override and the port reads
// as an UnusableSettingsError (CRW-1119, intentionally changed): each read of them must fail with that error, the store untouched;
// their writes keep the oracle's answers.
var oracleUnusableReads = map[string]bool{
	"read_not_json": true, "read_roles_array": true, "read_roles_null": true, "read_roles_string": true, "read_top_array": true,
	"read_top_null": true, "read_top_string": true, "read_top_number": true, "read_top_true": true, "read_empty_file": true,
	"read_trailing_garbage": true, "read_bom": true, "read_partial_values": true, "read_role_shapes": true,
	"read_fallback_shapes": true, "store_is_directory": true,
}

// oracleIntent is the recorded op as the port answers it where CRW-1119 changes the answer on purpose: model_whitespace stores a
// whitespace-only primary model in the oracle and is refused here (trimmed, as a fallback model is), so nothing is ever written.
func oracleIntent(id string, i int, op recordedOp) recordedOp {
	if id != "model_whitespace" {
		return op
	}
	op.File = nil
	switch i {
	case 0:
		refusal := "mode \"model\" requires a non-empty model id"
		op.Error, op.Result = &refusal, nil
	case 1:
		defaults := string(must(Stringify(DefaultConfig(), "")))
		op.Result = &defaults
	}
	return op
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
				op = oracleIntent(c.ID, i, op)
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
				var unusable *UnusableSettingsError
				switch {
				case oracleUnusableReads[c.ID] && (op.Op == "settings" || op.Op == "config"):
					if !errors.As(err, &unusable) {
						t.Fatalf("%s: %v, want an UnusableSettingsError (CRW-1119)", at, err)
					}
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
