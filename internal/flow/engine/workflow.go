package engine

import (
	"encoding/json"
	"fmt"

	"github.com/dop251/goja"
)

// workflow is workflow(nameOrRef, args): it runs another script inline as one step and resolves with what it
// returns. The child shares the run's pool, journal, budget and abort; its calls live on the child path of the
// position workflow() took. Nesting is one level: workflow() inside a child throws.
func (s *scriptVM) workflow(call goja.FunctionCall) goja.Value {
	s.live()
	if s.depth >= 1 {
		s.throwType("workflow(): nesting is one level only")
	}
	if s.e.cfg.Workflows == nil {
		s.throwType("workflow(): this run has no workflow registry")
	}
	var name, scriptPath string
	switch ref := call.Argument(0).(type) {
	case *goja.Object:
		scriptPath = s.optString(ref, "scriptPath")
		if scriptPath == "" {
			s.throwType("workflow(): a reference object needs a scriptPath")
		}
	default:
		str, ok := ref.Export().(string)
		if !ok || str == "" {
			s.throwType("workflow(): the first argument is a workflow name or {scriptPath}")
		}
		name = str
	}
	var childArgs json.RawMessage
	if !goja.IsUndefined(call.Argument(1)) {
		a, err := s.jsonOfErr(call.Argument(1))
		if err != nil {
			s.throwType("workflow(): the arguments cannot be serialised: %v", err)
		}
		childArgs = a
	}
	prog, err := s.e.cfg.Workflows(name, scriptPath)
	if err != nil {
		panic(s.vm.NewGoError(fmt.Errorf("workflow(): %w", err)))
	}
	label := "workflow:" + name + scriptPath
	path, position := s.open(label)
	id := s.e.callID(path.id, position)
	s.e.j.Append(Event{RunID: s.e.runID, Kind: KindFlow, Path: path.id, Position: position, Op: "workflow", Label: label, Event: "workflow_opened",
		Data: map[string]any{"call_id": id, "script_digest": prog.Digest}})

	promise, resolve, reject := s.vm.NewPromise()
	complete := s.e.loop.Begin(s.rt)
	settle := func(ok bool, jsonText []byte, msg string) {
		complete(func() {
			path.settle(position)
			s.e.j.Append(Event{RunID: s.e.runID, Kind: KindFlow, Path: path.id, Position: position, Op: "workflow", Label: label, Event: "workflow_settled"})
			var err error
			if ok {
				err = resolve(s.fromJSON(jsonText))
			} else {
				err = reject(s.vm.NewGoError(fmt.Errorf("%s", msg)))
			}
			if err != nil {
				s.e.failResolve(err)
			}
		})
	}
	child, err := s.e.newScriptVM(prog, path.child(position, "workflow"), s.depth+1, childArgs)
	if err != nil {
		settle(false, nil, "workflow(): "+err.Error())
		return s.vm.ToValue(promise)
	}
	cp, err := child.start()
	if err != nil {
		settle(false, nil, "workflow(): "+err.Error())
		return s.vm.ToValue(promise)
	}
	cpObj := child.vm.ToValue(cp).ToObject(child.vm)
	then, _ := goja.AssertFunction(cpObj.Get("then"))
	onOK := child.vm.ToValue(func(c goja.FunctionCall) goja.Value {
		v, err := child.jsonOfErr(c.Argument(0))
		if err != nil {
			settle(false, nil, "workflow(): the child's value cannot be serialised: "+err.Error())
			return goja.Undefined()
		}
		settle(true, v, "")
		return goja.Undefined()
	})
	onErr := child.vm.ToValue(func(c goja.FunctionCall) goja.Value {
		settle(false, nil, "workflow(): the child script failed: "+child.errorText(c.Argument(0)))
		return goja.Undefined()
	})
	if _, err := then(cpObj, onOK, onErr); err != nil {
		settle(false, nil, "workflow(): "+err.Error())
	}
	return s.vm.ToValue(promise)
}
