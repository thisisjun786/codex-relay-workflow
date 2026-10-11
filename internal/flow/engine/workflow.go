package engine

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dop251/goja"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
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
	// Only the scripts of this run can settle the child, so waiting for it does not keep the loop running: a child
	// that awaits something nothing will settle ends the run as unsettled.
	complete := s.e.loop.Link(s.rt)
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
	// Everything that touches the child's VM runs under the child's step and heap limits: registering the reactions
	// (the child may have replaced what then reads) and rendering its value or error. The reactions themselves only
	// hand the settled value to the loop, which renders it inside child.rt.Enter, also when the child had settled
	// already; the parent's watchdog belongs to another VM.
	onOK := child.vm.ToValue(func(c goja.FunctionCall) goja.Value {
		val := c.Argument(0)
		s.e.loop.Post(child.rt, func() {
			v, err := child.jsonOfErr(val)
			if err != nil {
				settle(false, nil, "workflow(): the child's value cannot be serialised: "+err.Error())
				return
			}
			settle(true, v, "")
		})
		return goja.Undefined()
	})
	onErr := child.vm.ToValue(func(c goja.FunctionCall) goja.Value {
		val := c.Argument(0)
		s.e.loop.Post(child.rt, func() {
			settle(false, nil, "workflow(): the child script failed: "+child.errorText(val))
		})
		return goja.Undefined()
	})
	if err := child.rt.Enter(func() error {
		cpObj := child.vm.ToValue(cp).ToObject(child.vm)
		then, ok := goja.AssertFunction(cpObj.Get("then"))
		if !ok {
			return errors.New("the child's promise has no then")
		}
		_, err := then(cpObj, onOK, onErr)
		return err
	}); err != nil {
		var le *script.LimitError
		if errors.As(err, &le) {
			s.e.fail(limitKind(err), err)
			return s.vm.ToValue(promise)
		}
		settle(false, nil, "workflow(): "+err.Error())
	}
	return s.vm.ToValue(promise)
}
