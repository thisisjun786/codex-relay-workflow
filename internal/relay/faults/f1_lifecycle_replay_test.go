package faults

import (
	"testing"
)

func TestF1_FLT_25_26_27_LifecycleWholeCLI(t *testing.T) {
	testFLT252627LifecycleWholeCLI(t)
}

func testFLT252627LifecycleWholeCLI(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	invoke := func(args ...string) map[string]any { return f1ReplayCLI(t, ctx, gd, args) }
	invoke("fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P")
	observed := invoke("fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"flow"},"occurrenceKey":"first","scope":{"projectKey":"P"}}`)
	id := observed["faultId"].(string)
	pub := observed["publication"].(map[string]any)["publicationId"].(string)
	first := invoke("fault-queue", "--fault", id, "--kind", "append_comment", "--trigger", "fix:one")
	comment := first["publicationId"].(string)
	invoke("fault-queue", "--fault", id, "--kind", "append_comment", "--trigger", "fix:one")
	invoke("fault-claim", "--publication", comment, "--owner", "operator")
	invoke("fault-claim", "--publication", pub, "--owner", "operator")
	invoke("fault-reconcile", "--publication", pub, "--searched")
	operation := invoke("fault-operation", "--publication", pub, "--claim-token", "0001020304050607")
	block := operation["block"].(string)
	invoke("fault-reconcile", "--publication", pub, "--searched")
	invoke("fault-fail", "--publication", pub, "--claim-token", "0001020304050607", "--error", "response lost")
	invoke("fault-claim", "--publication", pub, "--owner", "operator")
	invoke("fault-reconcile", "--publication", pub)
	invoke("fault-reconcile", "--publication", pub, "--searched")
	invoke("fault-reconcile", "--publication", pub, "--observed", block)
	invoke("fault-reconcile", "--publication", pub, "--searched", "--prior-ended", "--reason", "request ended")
	invoke("fault-complete", "--publication", pub, "--readback", block, "--external-ref", "ISSUE", "--project-ref", "project-P")
	invoke("fault-claim", "--publication", pub, "--owner", "operator")
	operation = invoke("fault-operation", "--publication", pub, "--claim-token", "0001020304050607")
	block = operation["block"].(string)
	invoke("fault-fail", "--publication", pub, "--claim-token", "0001020304050607", "--error", "response lost")
	invoke("fault-complete", "--publication", pub, "--readback", block, "--external-ref", "ISSUE", "--project-ref", "project-P")
	invoke("fault-claim", "--publication", comment, "--owner", "operator")
	operation = invoke("fault-operation", "--publication", comment, "--claim-token", "0001020304050607")
	invoke("fault-complete", "--publication", comment, "--claim-token", "0001020304050607", "--readback", operation["block"].(string), "--external-ref", "OTHER")
	invoke("fault-complete", "--publication", comment, "--claim-token", "0001020304050607", "--readback", operation["block"].(string))
}

func TestF1ArgumentFilesWholeCLI(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	for _, args := range [][]string{
		{"fault-sweep", "--readings", "@" + gd + "/missing"},
		{"fault-reconcile", "--publication", "missing", "--observed", "@" + gd + "/missing"},
		{"fault-complete", "--publication", "missing", "--readback", "@" + gd + "/missing"},
		{"fault-complete", "--publication", "missing", "--observed-fields", "@" + gd + "/missing"},
	} {
		f1ReplayCLI(t, ctx, gd, args)
	}
}
