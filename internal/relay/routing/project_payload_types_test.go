package routing

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test23ProjectPayloadWrongTypesMatchPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	script := `import json,sys
from codex_session_relay import projects
base={'product':'p','workspace':'w','team':'t','familyLabel':'f','goal':'g','criteria':'c','name':'n','members':['m1','m2'],'components':['c1']}
out=[]
for field in ('team','name','lead','members','components'):
 for value in (123,True,None,[],{}):
  one=dict(base); one[field]=value
  out.append([field,value,projects._validate(one)])
for field in ('members','components'):
 for value in (123,True,None,[],{},'', '  '):
  one=dict(base); one[field]=(['m1',value] if field == 'members' else ['c1',value])
  out.append([field+'-item',one[field],projects._validate(one)])
print(json.dumps(out))`
	raw := pyoracle.Answer(t, "projects._validate wrong types", func() ([]byte, error) {
		cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script)
		cmd.Dir = root
		return cmd.Output()
	})
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var cases []struct {
		Field    string
		Value    any
		Problems []string
	}
	var records [][]any
	if err := decoder.Decode(&records); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		cases = append(cases, struct {
			Field    string
			Value    any
			Problems []string
		}{record[0].(string), record[1], func() []string {
			var out []string
			for _, v := range record[2].([]any) {
				out = append(out, v.(string))
			}
			return out
		}()})
	}
	base := map[string]any{"product": "p", "workspace": "w", "team": "t", "familyLabel": "f", "goal": "g", "criteria": "c", "name": "n", "members": []any{"m1", "m2"}, "components": []any{"c1"}}
	for _, tc := range cases {
		t.Run(tc.Field+"/"+evidence.TypeName(tc.Value), func(t *testing.T) {
			one := make(map[string]any, len(base)+1)
			for k, v := range base {
				one[k] = v
			}
			field := strings.TrimSuffix(tc.Field, "-item")
			one[field] = tc.Value
			if got := ValidateProjectPayload(one); !reflect.DeepEqual(got, tc.Problems) {
				t.Fatalf("queue/pre-issue validator differs: Go=%v Python=%v", got, tc.Problems)
			}
		})
	}
}
