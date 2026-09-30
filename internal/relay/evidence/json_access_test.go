package evidence

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Envelope helpers are library surfaces, not commands. Compare their real Python
// implementation, including error classes/details, rather than invented CLI flags.
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
		t.Run(tc.Op+"/"+Repr(tc.Value), func(t *testing.T) {
			var got any
			err := func() (err error) {
				defer RecoverPython(&err)
				ladder, _ := Unreached(ChildToParent)
				ladder[TransportAccepted] = tc.Value
				switch tc.Op {
				case "shown":
					got = Shown(tc.Value)
				case "detail":
					got = Shown(map[string]any{"absent": "unknown", "detail": tc.Value})
				case "holds":
					got = StageHolds(ladder, TransportAccepted)
				case "promotion":
					got = PromotionRefused(ladder)
					if got.([]string) == nil {
						got = []string{}
					}
				case "source":
					ladder[TransportAccepted] = map[string]any{"state": "yes", "source": tc.Value}
					err = CheckReach(ChildToParent, ladder)
				case "reach":
					err = CheckReach(ChildToParent, ladder)
				}
				return err
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
