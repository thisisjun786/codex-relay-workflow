package supervisor

import (
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func Test24PacketAccessorPython(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	row, err := f.c.Get(f.ctx, staged["messageId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `import json,sys,copy
from codex_session_relay import supervisorchannel as s
s.relay_program=lambda: ["/usr/bin/codex-session-relay"]
channel=object.__new__(s.SupervisorChannel)
channel.state_directory=sys.argv[1];channel.socket_path=None
packet=json.load(sys.stdin);out=[]
for field in ("kind","sender","basis","evidence","observedAt","artifact","generation"):
 for value in [None,False,True,0,2,1.5,"","x",[],[1],{}, {"a":1}]:
  p=copy.deepcopy(packet)
  target=p if field in ("artifact","generation") else p["envelope"]
  target[field]=value
  try: result=channel.render(p,"request","token");error=None
  except Exception as ex: result=None;error=type(ex).__name__+": "+str(ex)
  out.append({"field":field,"value":value,"result":result,"error":error})
print(json.dumps(out))`
	cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script, f.c.StoreDirectory())
	cmd.Stdin = strings.NewReader(row.Packet)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle: %v\n%s", err, raw)
	}
	for _, one := range evidence.Items(evidence.Decode(string(raw))) {
		tc := evidence.Dict(one, false)
		field := evidence.Text(tc["field"])
		t.Run(field+"/"+evidence.Repr(tc["value"]), func(t *testing.T) {
			p := Packet(evidence.Dict(evidence.Decode(row.Packet), false))
			if field == "artifact" || field == "generation" {
				p[field] = tc["value"]
			} else {
				envelope := evidence.Dict(p["envelope"], false)
				envelope[field] = tc["value"]
				p["envelope"] = envelope
			}
			var got any
			err := func() (err error) {
				defer evidence.RecoverPython(&err)
				got = f.c.render(p, "request", "token")
				return nil
			}()
			var failure any
			if err != nil {
				failure = err.Error()
			}
			if evidence.Dumps(failure, false, true, true) != evidence.Dumps(tc["error"], false, true, true) || (err == nil && got != tc["result"]) {
				t.Fatalf("diff: Go=(%v,%v) Python=(%v,%v)", got, failure, tc["result"], tc["error"])
			}
		})
	}
}

func Test24MalformedProposalRollsBackClaim(t *testing.T) {
	for _, packet := range []string{`{"envelope":true}`, `{"envelope":[]}`, `{}`} {
		t.Run(packet, func(t *testing.T) {
			f := fixture24(t)
			_, staged := f.staged(t)
			id := staged["messageId"].(string)
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", packet, id); err != nil {
				t.Fatal(err)
			}
			before, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.c.Resolve(f.ctx, before.RelationshipID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.c.claim(f.ctx, id, r, 1700000000, f.at, "test")
			var failure *evidence.PythonError
			if !errors.As(err, &failure) {
				t.Fatalf("expected Python error, got %T %v", err, err)
			}
			after, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatalf("claim changed message: %s -> %s", a, b)
			}
			attempts, err := f.s.SupervisorAttempts(f.ctx, id)
			if err != nil || len(attempts) != 0 {
				t.Fatalf("claim wrote attempts: %v %v", attempts, err)
			}
		})
	}
}
