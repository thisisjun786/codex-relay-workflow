package supervisor

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// These library validators have no production CLI caller in this checkout.
// Exercise the actual Python validators rather than inventing a report CLI.
func Test24ReportAccessorPython(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `import json
from codex_session_relay import report,cxc
from codex_session_relay.errors import ReceiptRefused
values=[None,False,True,0,2,1.5,"","x",[],[1],{}, {"a":1}]
out=[]
for v in values:
 for field in ("status","kind","disposition","addressedBy","followUpOwner","reopenTrigger"):
  entry={"threadId":"T","disposition":"not_applicable","evidence":"checked"}
  entry[field]=v
  try:
   if field=="status": result=cxc.check_status(v,"blocked_needs_input")
   elif field=="kind": result=report._check_review({"kind":v})
   else: result=report._check_dispositions([entry],{"threadsSeen":["T"]})
   error=None
  except ReceiptRefused as e: result=None;error={"reason":e.reason.value,"detail":e.detail}
  except Exception as e: result=None;error=type(e).__name__+": "+str(e)
  out.append({"value":v,"field":field,"result":result,"error":error})
print(json.dumps(out))`
	cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle: %v %s", err, raw)
	}
	var cases []struct {
		Value  any
		Field  string
		Result any
		Error  any
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err = decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Field+"/"+evidence.Repr(tc.Value), func(t *testing.T) {
			var got any
			var err error
			switch tc.Field {
			case "status":
				err = checkReportStatus(tc.Value, "blocked_needs_input")
			case "kind":
				got, err = validateReportReview(map[string]any{"kind": tc.Value})
			default:
				entry := map[string]any{"threadId": "T", "disposition": "not_applicable", "evidence": "checked", tc.Field: tc.Value}
				got, err = validateThreadDispositions([]any{entry}, map[string]any{"threadsSeen": []any{"T"}})
			}
			if evidence.Dumps(reportRefusalValue(err), false, true, true) != evidence.Dumps(tc.Error, false, true, true) {
				t.Fatalf("error diff Go=%v Python=%v", reportRefusalValue(err), tc.Error)
			}
			if err == nil && evidence.Dumps(got, false, true, true) != evidence.Dumps(tc.Result, false, true, true) {
				t.Fatalf("value diff Go=%v Python=%v", got, tc.Result)
			}
		})
	}
}

func Test24ReportAccessorRefusalWritesNothing(t *testing.T) {
	f := fixture24(t)
	before, err := f.s.All(context.Background(), "SELECT * FROM work_reports")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{nil, true, 2, 1.5, []any{1}, map[string]any{"a": 1}} {
		_, err := RecordWorkReport(f.ctx, f.s, delivery.NewFakeClock(), f.event, map[string]any{"cxc_status": value})
		if err == nil {
			t.Fatalf("accepted wrong status %v", value)
		}
	}
	after, err := f.s.All(context.Background(), "SELECT * FROM work_reports")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatalf("refusal changed reports: %s -> %s", a, b)
	}
}
