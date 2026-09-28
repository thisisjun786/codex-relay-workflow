package adapter

import (
	"testing"
)

const parentLoaded = "01parent-loaded-elsewhere"
const rootsCWD = "/workspace/example/tasks/PA"

var recordedRoots = []any{rootsCWD, "/workspace/example/run", "/workspace/example/evidence", "/workspace/example/relay-state"}

func rootScenario(write bool, status string, roots []any) scenario {
	settings := authorized()
	settings["cwd"] = rootsCWD
	settings["runtimeWorkspaceRoots"] = recordedRoots
	settings["environments"] = []any{map[string]any{"environmentId": "local", "cwd": rootsCWD, "runtimeWorkspaceRoots": recordedRoots}}
	settings["sandbox"] = map[string]any{"type": "dangerFullAccess"}
	if write {
		settings["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": recordedRoots[1:], "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	}
	response := map[string]any{}
	for k, v := range settings {
		if k != "environments" {
			response[k] = v
		}
	}
	response["runtimeWorkspaceRoots"] = roots
	response["activePermissionProfile"] = nil
	response["approvalsReviewer"] = "user"
	response["thread"] = map[string]any{"id": parentLoaded, "status": map[string]any{"type": "idle"}, "environments": []any{map[string]any{"environmentId": "local", "cwd": rootsCWD, "runtimeWorkspaceRoots": roots}}, "cwd": rootsCWD, "model": settings["model"], "reasoningEffort": "xhigh"}
	if write {
		writable := []any{}
		for _, root := range roots {
			if root != rootsCWD {
				writable = append(writable, root)
			}
		}
		response["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": writable, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	}
	return scenario{settings: settings, answers: []map[string]any{{"thread": map[string]any{"id": parentLoaded, "status": map[string]any{"type": status}}}, response, {"turn": map[string]any{"id": "turn-1"}}}, actions: [][]any{{"send", "del-235000000000-a1", parentLoaded, "the child's report"}}}
}
func Test28_BLR_1_LoadedRootNarrowing(t *testing.T) {
	for _, write := range []bool{false, true} {
		capture(t, rootScenario(write, "idle", []any{rootsCWD}))
	}
	s := rootScenario(false, "idle", []any{rootsCWD})
	s.settingsFree = true
	capture(t, s)
}
func Test28_BLR_2_ExactLoads(t *testing.T) {
	capture(t, rootScenario(false, "idle", []any{recordedRoots[3], recordedRoots[2], recordedRoots[1], recordedRoots[0]}))
	capture(t, rootScenario(false, "notLoaded", recordedRoots))
	capture(t, rootScenario(false, "idle", recordedRoots))
}
func Test28_BLR_3_WiderOrUnloadedRootsWithhold(t *testing.T) {
	capture(t, rootScenario(false, "idle", []any{rootsCWD, "/workspace/example/elsewhere"}))
	s := rootScenario(false, "idle", []any{rootsCWD})
	s.answers[1]["thread"].(map[string]any)["environments"] = []any{map[string]any{"environmentId": "local", "cwd": rootsCWD, "runtimeWorkspaceRoots": []any{rootsCWD, "/workspace/example/elsewhere"}}}
	capture(t, s)
	capture(t, rootScenario(false, "notLoaded", []any{rootsCWD}))
	s = rootScenario(true, "notLoaded", recordedRoots)
	s.answers[1]["sandbox"].(map[string]any)["writableRoots"] = []any{"/workspace/example/run"}
	capture(t, s)
	for _, change := range []map[string]any{{"model": "someone/else"}, {"reasoningEffort": "low"}} {
		s = rootScenario(false, "idle", recordedRoots)
		for k, v := range change {
			s.answers[1][k] = v
		}
		capture(t, s)
	}
	for _, change := range []map[string]any{{"networkAccess": true}, {"writableRoots": append(append([]any{}, recordedRoots[1:]...), "/workspace/example/elsewhere")}} {
		s = rootScenario(true, "idle", recordedRoots)
		for k, v := range change {
			s.answers[1]["sandbox"].(map[string]any)[k] = v
		}
		capture(t, s)
	}
	s = rootScenario(false, "idle", recordedRoots)
	s.answers[1]["sandbox"].(map[string]any)["writableRoots"] = []any{"/workspace/example/elsewhere"}
	capture(t, s)
	s = rootScenario(false, "idle", recordedRoots)
	s.settings["sandbox"] = map[string]any{"type": "dangerFullAccess", "writableRoots": []any{"/workspace/example/run", "/workspace/example/evidence"}}
	s.answers[1]["sandbox"] = map[string]any{"type": "dangerFullAccess", "writableRoots": []any{"/workspace/example/run"}}
	capture(t, s)
	s = rootScenario(false, "idle", []any{rootsCWD})
	s.answers[1]["model"] = "someone/else"
	capture(t, s)
}
func Test28_TPH_1_EstablishmentBeforeReadOrResume(t *testing.T) {
	for _, index := range []int{0, 1} {
		s := sendScenario(resume(), []any{"send", "req-establish-read", "thread-1", "hello"})
		s.answers[index] = map[string]any{"phase": "establish"}
		capture(t, s)
	}
}
func Test28_TPH_2_TurnEstablishmentTransmitAndAckUncertain(t *testing.T) {
	for _, phase := range []string{"establish", "transmit", "ack"} {
		s := sendScenario(resume(), []any{"send", "req-" + phase, "thread-1", "hello"})
		s.answers[2] = map[string]any{"phase": phase}
		capture(t, s)
	}
}
