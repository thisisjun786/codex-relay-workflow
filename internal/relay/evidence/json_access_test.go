package evidence

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Shown is the one envelope helper the product reads (the supervisor channel's rendered message).
// Compare it with the real Python implementation over every JSON value shape, directly and as an
// absence's detail; the recorded answer also holds the reach-ladder helpers retired in wave R1.
func Test24EnvelopeAccessorPython(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `import json
from codex_session_relay import envelope as e
out=[]
for v in [None,False,True,0,2,1.5,"","x",[],[1],{}, {"a":1}]:
 for op in ("shown","detail","holds","promotion","reach","source"):
  ladder=e.unreached(e.CHILD_TO_PARENT)
  ladder[e.TRANSPORT_ACCEPTED]=v
  try:
   if op=="shown": result=e.shown(v)
   elif op=="detail": result=e.shown({"absent":"unknown","detail":v})
   elif op=="holds": result=e.stage_holds(ladder,e.TRANSPORT_ACCEPTED)
   elif op=="promotion": result=e.promotion_refused(ladder)
   else:
    if op=="source": ladder[e.TRANSPORT_ACCEPTED]={"state":"yes","source":v}
    result=e.check_reach(e.CHILD_TO_PARENT,ladder)
   error=None
  except e.EnvelopeRefused as ex: result=None;error=ex.detail
  except Exception as ex: result=None;error=type(ex).__name__+": "+str(ex)
  out.append({"op":op,"value":v,"result":result,"error":error})
print(json.dumps(out))`
	raw := pyoracle.Answer(t, "envelope-accessors", func() ([]byte, error) {
		raw, err := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("oracle: %v\n%s", err, raw)
		}
		return raw, nil
	})
	for _, value := range Items(Decode(string(raw))) {
		row := Dict(value, false)
		tc := struct {
			Op                   string
			Value, Result, Error any
		}{Text(row["op"]), row["value"], row["result"], row["error"]}
		if tc.Op != "shown" && tc.Op != "detail" {
			continue
		}
		t.Run(tc.Op+"/"+Repr(tc.Value), func(t *testing.T) {
			var got any
			err := func() (err error) {
				defer RecoverPython(&err)
				if tc.Op == "shown" {
					got = Shown(tc.Value)
				} else {
					got = Shown(map[string]any{"absent": "unknown", "detail": tc.Value})
				}
				return nil
			}()
			var failure any
			if err != nil {
				failure = err.Error()
			}
			if Dumps(failure, false, true, true) != Dumps(tc.Error, false, true, true) || (err == nil && Dumps(got, false, true, true) != Dumps(tc.Result, false, true, true)) {
				t.Fatalf("diff: Go=(%v,%v) Python=(%v,%v)", got, failure, tc.Result, tc.Error)
			}
		})
	}
}
