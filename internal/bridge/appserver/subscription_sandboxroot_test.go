package appserver

import (
	"context"
	"testing"
)

// CRW-1265. The subscription-only resume (HoldThread) is outside the sandbox-root rule because it
// starts nothing: it names the thread and asks for no turns, it sets no cwd, sandbox, writable roots,
// model or approval policy, and it sends no turn/start. The paths that really run the thread again
// (a bridge send, child-resume, a relay delivery) are judged where they send their own resume. This
// test pins the fields, so a field that could change where the thread runs cannot be added unseen.
func TestSubscriptionResumeSendsNoCwdSandboxOrTurn(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	params := holdParams(t, host)
	if len(params) != 2 || params["threadId"] != "thread-1" || params["excludeTurns"] != true {
		t.Fatalf("the subscription resume carries %v, want only threadId and excludeTurns=true", params)
	}
	for _, field := range []string{"cwd", "sandbox", "runtimeWorkspaceRoots", "approvalPolicy", "model", "config", "permissions", "activePermissionProfile"} {
		if _, present := params[field]; present {
			t.Fatalf("the subscription resume carries %s", field)
		}
	}
	for _, method := range []string{"turn/start", "thread/start", "turn/steer"} {
		if got := host.Count(method); got != 0 {
			t.Fatalf("the subscription resume sent %d %s", got, method)
		}
	}
	if got := host.Count("thread/resume"); got != 1 {
		t.Fatalf("%d resumes reached the host, want 1", got)
	}
}
