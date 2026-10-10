package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tools gives a fake ocx the system tools; providerPath leaves PATH holding only the fake itself.
func tools(body string) string { return "PATH=/usr/bin:/bin\n" + body }

func shortBudget(t *testing.T, d time.Duration) {
	t.Helper()
	saved := statusTimeout
	statusTimeout = d
	t.Cleanup(func() { statusTimeout = saved })
}

// runProbe runs the provider command and answers its exit status, its stdout line decoded and how long it took. A
// probe that never returns fails the test instead of hanging it.
func runProbe(t *testing.T, ctx context.Context, args ...string) (code int, line map[string]any, raw string, took time.Duration) {
	t.Helper()
	var out, errOut bytes.Buffer
	done := make(chan struct{})
	start := time.Now()
	go func() { defer close(done); code = Run(ctx, args, &out, &errOut) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the provider command did not return in 20 s")
	}
	took = time.Since(start)
	raw = out.String()
	_ = json.Unmarshal([]byte(raw), &line)
	return
}

func TestStatusPipeHeldByAChildEndsWithinTheBudget(t *testing.T) {
	// ocx prints a complete payload and exits 0, but leaves a child holding its stdout and stderr.
	providerPath(t, tools(`printf '%s\n' '{"proxy":{"running":true},"defaultProvider":"openai","listen":{"port":10100}}'
sleep 4 &
exit 0`))
	shortBudget(t, time.Second)
	code, line, raw, took := runProbe(t, context.Background())
	if code != 0 || line["mode"] != "provider" || line["running"] != true {
		t.Fatalf("%d %q", code, raw)
	}
	if took > 2500*time.Millisecond {
		t.Fatalf("the probe took %s with a 1 s budget and a %s grace", took, statusGrace)
	}
}

func TestStatusNeverFinishingOcxIsAnErrorWithATimeoutDiagnosis(t *testing.T) {
	// The shell waits on a foreground child that outlives the budget and holds the pipes.
	providerPath(t, tools("sleep 4"))
	shortBudget(t, time.Second)
	code, line, raw, took := runProbe(t, context.Background())
	if code != 0 || line["mode"] != "error" || !strings.Contains(line["reason"].(string), "timed out after 1s") {
		t.Fatalf("%d %q", code, raw)
	}
	if took > 2500*time.Millisecond {
		t.Fatalf("the probe took %s", took)
	}
}

func TestStatusPartialJSONThenHangIsAnError(t *testing.T) {
	providerPath(t, tools(`printf '{"proxy":{"run'
sleep 4`))
	shortBudget(t, time.Second)
	code, line, raw, took := runProbe(t, context.Background())
	if code != 0 || line["mode"] != "error" || !strings.Contains(line["reason"].(string), "timed out") {
		t.Fatalf("%d %q", code, raw)
	}
	if took > 2500*time.Millisecond {
		t.Fatalf("the probe took %s", took)
	}
}

func TestStatusPartialJSONFromAnExitedOcxWhoseChildHoldsThePipe(t *testing.T) {
	providerPath(t, tools(`printf '{"proxy":{"run'
sleep 4 &
exit 0`))
	shortBudget(t, time.Second)
	code, line, raw, took := runProbe(t, context.Background())
	if code != 0 || line["mode"] != "error" || !strings.Contains(line["reason"].(string), "pipe") {
		t.Fatalf("%d %q", code, raw)
	}
	if took > 2500*time.Millisecond {
		t.Fatalf("the probe took %s", took)
	}
}

func TestStatusOutputOverTheLimitIsAnError(t *testing.T) {
	providerPath(t, tools("head -c 3000000 /dev/zero | tr '\\0' x"))
	code, line, raw, _ := runProbe(t, context.Background())
	if code != 0 || line["mode"] != "error" || !strings.Contains(line["reason"].(string), "output exceeded 1048576 bytes") {
		t.Fatalf("%d %.200q", code, raw)
	}
}

func TestStatusCancellationEndsTheProbeWithoutALine(t *testing.T) {
	providerPath(t, tools("sleep 4"))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	code, _, raw, took := runProbe(t, ctx)
	if code != 130 || raw != "" || took > 2500*time.Millisecond {
		t.Fatalf("%d %q after %s", code, raw, took)
	}
}

func TestStatusFastAnswerDoesNotWaitForTheGrace(t *testing.T) {
	providerPath(t, tools(`printf '%s\n' '{"proxy":{"running":false}}'`))
	code, line, raw, took := runProbe(t, context.Background())
	if code != 0 || line["mode"] != "provider" || line["running"] != false {
		t.Fatalf("%d %q", code, raw)
	}
	if took > statusGrace {
		t.Fatalf("a status that ended at once took %s", took)
	}
}

func TestHelpAndUnknownVerbsAnswerBeforeTheProbe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probed")
	t.Setenv("MARKER", marker)
	providerPath(t, tools(`touch "$MARKER"
printf '%s\n' '{"proxy":{"running":true}}'`))
	probed := func() bool { _, err := os.Stat(marker); return err == nil }
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"--help", "ignored"}} {
		code, _, raw, _ := runProbe(t, context.Background(), args...)
		if code != 0 || raw != Help+"\n" || probed() {
			t.Fatalf("%v: %d %q probed=%v", args, code, raw, probed())
		}
	}
	for _, args := range [][]string{{"frobnicate"}, {"--wat"}, {"extra", "detect"}} {
		var out, errOut bytes.Buffer
		if code := Run(context.Background(), args, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "crw provider:") || !strings.Contains(errOut.String(), "--help") || probed() {
			t.Fatalf("%v: %d %q %q probed=%v", args, code, out.String(), errOut.String(), probed())
		}
	}
	for _, args := range [][]string{nil, {"detect"}, {"status"}, {"detect", "--help"}, {"status", "ignored"}} {
		_ = os.Remove(marker)
		code, line, raw, _ := runProbe(t, context.Background(), args...)
		if code != 0 || line["mode"] != "provider" || !probed() {
			t.Fatalf("%v: %d %q probed=%v", args, code, raw, probed())
		}
	}
}

func TestListenPortMustBeAValidTCPPort(t *testing.T) {
	zero := 0
	status := func(listen string) Status {
		return Detect(Deps{Which: func(string) string { return "/fake/ocx" }, RunStatus: func(string) (*int, string, error) {
			return &zero, `{"proxy":{"running":true},"listen":` + listen + `}`, nil
		}})
	}
	for _, c := range []struct {
		listen string
		port   string // the advertised port, "null" when none
	}{
		{`{"port":1}`, "1"}, {`{"port":10100}`, "10100"}, {`{"port":65535}`, "65535"}, {`{"port":8080.0}`, "8080"}, {`{"port":1e3}`, "1000"},
		{`{}`, "null"}, {`{"port":null}`, "null"}, {`{"port":"1"}`, "null"}, {`null`, "null"},
	} {
		if got := Line(status(c.listen)); !strings.Contains(got, `"mode":"provider"`) || !strings.HasSuffix(got, `,"port":`+c.port+`}`) {
			t.Errorf("%s: %s", c.listen, got)
		}
	}
	for _, c := range []struct{ listen, shown string }{
		{`{"port":0}`, "0"}, {`{"port":-1}`, "-1"}, {`{"port":65536}`, "65536"}, {`{"port":1.5}`, "1.5"}, {`{"port":1e400}`, "1e400"}, {`{"port":-0}`, "-0"}, {`{"port":1e9}`, "1e9"},
	} {
		got := Line(status(c.listen))
		if !strings.Contains(got, `"mode":"error"`) || !strings.Contains(got, `"reason":"ocx status reported a listen.port that is not a TCP port: `+c.shown+`"`) {
			t.Errorf("%s: %s", c.listen, got)
		}
	}
}
