package contracttest

import (
	"encoding/json"
	"testing"
)

// The closed network is the minimum a Go build can have: the case's proxy variables name an address
// nothing listens on. A fetch the oracle logged is replayed without being observed (the claim
// removes that call), and a scripted reply or a connect or dns observation, which nothing here
// provides, is refused.
func TestCXCReplay_closed_network(t *testing.T) {
	const proxy = "http://127.0.0.1:1"
	fetch := `[{"cmd":"fetch","argv":["GET","https://example.invalid/a"],"cwd":"${WS}"}]`
	closed := givenDoc(map[string]any{"files": map[string]string{"ws/script": `[ "$HTTP_PROXY $http_proxy $HTTPS_PROXY $https_proxy" = "` + proxy + " " + proxy + " " + proxy + " " + proxy + `" ] && printf 'closed\n'`}})
	removed := cxcClaim{State: cxcChanged, Remove: []string{"calls/0"}}
	scripted := givenDoc(map[string]any{"fetch": map[string]any{"https://example.invalid/a": map[string]any{"status": 200, "body": "{}"}}})
	runCXCRows(t, []cxcRow{
		{name: "the proxy variables name an address nothing listens on", given: closed, stdout: "a:x\ncwd:${WS}\nclosed\n"},
		{name: "a fetch the oracle logged is replayed with its call removed", calls: fetch, claim: removed},
		{name: "an identical claim fails at the call nothing observes", calls: fetch, err: "calls/0"},
		{name: "a scripted reply is refused", given: scripted, claim: removed, err: "scripted fetch reply"},
		{name: "an override that adds a scripted reply is refused", claim: cxcClaim{State: cxcChanged, Given: json.RawMessage(scripted)}, err: "scripted fetch reply"},
		{name: "an override that removes the scripted reply is replayed", given: scripted, claim: cxcClaim{State: cxcChanged, Given: json.RawMessage(`{"fetch":null}`)}},
		{name: "an expected connect is refused", calls: `[{"cmd":"connect","argv":["example.invalid","443"],"cwd":"${WS}"}]`, err: "connect or dns"},
		{name: "an expected dns lookup is refused", calls: `[{"cmd":"dns","argv":["example.invalid"],"cwd":"${WS}"}]`, claim: cxcClaim{State: cxcChanged}, err: "connect or dns"},
	})
}
