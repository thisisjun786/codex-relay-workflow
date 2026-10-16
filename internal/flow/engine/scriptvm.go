package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/dop251/goja"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// resultRef is what the engine remembers of a CallResult object it handed to a script: crw.classify and resume accept
// only these, so a bare object, a copy or null is refused.
type resultRef struct {
	res CallResult
}

// scriptVM is one script of a run (the main script, or the workflow it called) with its goja runtime and the path the
// code it is running belongs to.
type scriptVM struct {
	e     *Engine
	prog  *script.Program
	rt    *script.Runtime
	vm    *goja.Runtime
	depth int
	root  *pathState
	// cur is the path of the code running now. The async context tracker moves it with every await, so code that
	// resumes after an await belongs to the path it was running on when it awaited.
	cur *pathState

	args       json.RawMessage
	results    map[*goja.Object]*resultRef
	parse      goja.Callable
	stringify  goja.Callable
	freeze     goja.Callable
	toString   goja.Callable
	pathErrCls *goja.Object
}

type tracker struct{ s *scriptVM }

func (t tracker) Grab() any { return t.s.cur }
func (t tracker) Resumed(o any) {
	if p, ok := o.(*pathState); ok && p != nil {
		t.s.cur = p
		return
	}
	t.s.cur = t.s.root
}
func (t tracker) Exited() { t.s.cur = t.s.root }

func (e *Engine) newScriptVM(prog *script.Program, root *pathState, depth int, args json.RawMessage) (*scriptVM, error) {
	rt, err := prog.NewRuntimeOn(e.loop, e.cfg.Limits)
	if err != nil {
		return nil, err
	}
	s := &scriptVM{e: e, prog: prog, rt: rt, vm: rt.VM, depth: depth, root: root, cur: root, args: args, results: map[*goja.Object]*resultRef{}}
	s.vm.SetAsyncContextTracker(tracker{s})
	if err := s.install(); err != nil {
		return nil, err
	}
	return s, nil
}

// start calls the script body and returns the promise of its completion.
func (s *scriptVM) start() (*goja.Promise, error) {
	p, err := s.rt.Main()
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *scriptVM) throwType(format string, args ...any) {
	panic(s.vm.NewTypeError(append([]any{format}, args...)...))
}

// install defines the script's globals: agent, agent.call, parallel, pipeline, phase, log, args, budget, workflow and
// crw. No fs, net, exec or require exists.
func (s *scriptVM) install() error {
	vm := s.vm
	for _, name := range []string{"JSON.parse", "JSON.stringify", "Object.freeze", "String"} {
		v, err := vm.RunString(name)
		if err != nil {
			return err
		}
		fn, _ := goja.AssertFunction(v)
		switch name {
		case "JSON.parse":
			s.parse = fn
		case "JSON.stringify":
			s.stringify = fn
		case "String":
			s.toString = fn
		default:
			s.freeze = fn
		}
	}

	agent := vm.ToValue(func(call goja.FunctionCall) goja.Value { return s.agent(call, true) }).ToObject(vm)
	_ = agent.Set("call", func(call goja.FunctionCall) goja.Value { return s.agent(call, false) })
	_ = vm.Set("agent", agent)
	_ = vm.Set("phase", func(call goja.FunctionCall) goja.Value {
		title := call.Argument(0).String()
		s.e.phase = title
		s.e.phases = append(s.e.phases, title)
		s.e.j.Append(Event{RunID: s.e.runID, Kind: KindFlow, Path: s.cur.id, Event: "phase", Data: map[string]any{"title": title}})
		return goja.Undefined()
	})
	_ = vm.Set("log", func(call goja.FunctionCall) goja.Value {
		msg := call.Argument(0).String()
		s.e.logs = append(s.e.logs, msg)
		s.e.j.Append(Event{RunID: s.e.runID, Kind: KindFlow, Path: s.cur.id, Event: "log", Data: map[string]any{"message": msg}})
		return goja.Undefined()
	})

	var argsVal goja.Value = goja.Undefined()
	if len(s.args) > 0 && string(s.args) != "undefined" {
		v, err := s.parse(goja.Undefined(), vm.ToValue(string(s.args)))
		if err != nil {
			return fmt.Errorf("args: %w", err)
		}
		argsVal = v
	}
	_ = vm.Set("args", argsVal)

	b := vm.NewObject()
	if t := s.e.cfg.TokenBudget; t > 0 {
		_ = b.Set("total", t)
	} else {
		_ = b.Set("total", goja.Null())
	}
	// the budget the run's first process started with (StartedBudget), the same in every process of the run
	if t := s.e.runBudget; t > 0 {
		_ = b.Set("run", t)
	} else {
		_ = b.Set("run", goja.Null())
	}
	_ = b.Set("spent", func(goja.FunctionCall) goja.Value { return vm.ToValue(s.e.spent.Load()) })
	_ = b.Set("remaining", func(goja.FunctionCall) goja.Value {
		t := s.e.cfg.TokenBudget
		if t <= 0 {
			return vm.ToValue(math.Inf(1))
		}
		return vm.ToValue(max(0, t-s.e.spent.Load()))
	})
	_ = vm.Set("budget", b)

	_ = vm.Set("workflow", s.workflow)

	crw := vm.NewObject()
	_ = crw.Set("classify", s.classify)
	_ = crw.Set("clock", s.clock)
	_ = crw.Set("tierStep", s.tierStep)
	_ = crw.Set("callFacts", s.callFacts)
	_ = crw.Set("recordedSignal", s.recordedSignal)
	_ = crw.Set("priorRelease", s.priorRelease)
	_ = crw.Set("deferredAsks", s.deferredAsks)
	if err := s.installOps(crw); err != nil {
		return err
	}
	_ = vm.Set("crw", crw)

	x := vm.NewObject()
	_ = x.Set("openJoin", s.openJoin)
	_ = x.Set("runIn", s.runIn)
	_ = x.Set("closeJoin", s.closeJoin)
	_ = x.Set("itemFailed", s.itemFailed)
	_ = x.Set("setPathError", func(call goja.FunctionCall) goja.Value {
		s.pathErrCls = call.Argument(0).ToObject(vm)
		return goja.Undefined()
	})
	_ = vm.Set("__crw", x)
	if _, err := vm.RunString(bootstrap); err != nil {
		return fmt.Errorf("flow bootstrap: %w", err)
	}
	_ = vm.GlobalObject().Delete("__crw")
	return nil
}

// bootstrap defines parallel() and pipeline() on top of the engine's path primitives, and the error class of a
// refused second position.
const bootstrap = `(function (X) {
  class CrwPathConcurrencyError extends Error {
    constructor(message, info) {
      super(message);
      this.name = 'CrwPathConcurrencyError';
      this.path = info.path;
      this.openLabel = info.open;
      this.requestedLabel = info.requested;
    }
  }
  globalThis.CrwPathConcurrencyError = CrwPathConcurrencyError;
  X.setPathError(CrwPathConcurrencyError);

  function itemKeys(n, keys, what) {
    if (keys === undefined || keys === null) {
      const out = [];
      for (let i = 0; i < n; i++) out.push(String(i));
      return out;
    }
    if (!Array.isArray(keys) || keys.length !== n) throw new TypeError(what + ': opts.keys must list one key per item');
    const seen = new Set();
    return keys.map((k) => {
      k = String(k);
      if (seen.has(k)) throw new TypeError(what + ': duplicate item key ' + JSON.stringify(k));
      seen.add(k);
      return k;
    });
  }

  globalThis.parallel = function parallel(thunks, opts) {
    if (!Array.isArray(thunks)) throw new TypeError('parallel: the first argument must be an array of functions');
    for (const t of thunks) if (typeof t !== 'function') throw new TypeError('parallel: every item must be a function');
    const keys = itemKeys(thunks.length, opts && opts.keys, 'parallel');
    const label = opts && typeof opts.label === 'string' ? opts.label : 'parallel';
    const join = X.openJoin(label);
    const run = thunks.map((t, i) => X.runIn(join, keys[i], async () => {
      try { return await t(); } catch (e) { X.itemFailed(join, keys[i], e); return null; }
    }));
    return Promise.all(run).then((v) => { X.closeJoin(join, keys, v); return v; });
  };

  globalThis.pipeline = function pipeline(items, ...stages) {
    let opts = null;
    if (stages.length && typeof stages[stages.length - 1] !== 'function' && stages[stages.length - 1] != null) opts = stages.pop();
    if (!Array.isArray(items)) throw new TypeError('pipeline: the first argument must be an array');
    for (const s of stages) if (typeof s !== 'function') throw new TypeError('pipeline: every stage must be a function');
    const keys = itemKeys(items.length, opts && opts.keys, 'pipeline');
    const label = opts && typeof opts.label === 'string' ? opts.label : 'pipeline';
    const join = X.openJoin(label);
    const run = items.map((item, i) => X.runIn(join, keys[i], async () => {
      try {
        let prev = item;
        for (const st of stages) prev = await st(prev, item, i);
        return prev;
      } catch (e) { X.itemFailed(join, keys[i], e); return null; }
    }));
    return Promise.all(run).then((v) => { X.closeJoin(join, keys, v); return v; });
  };
})(__crw)`

// pathError builds the CrwPathConcurrencyError a script sees and ends the run with it.
func (s *scriptVM) pathError(err *PathConcurrencyError) *goja.Object {
	s.e.fail(ErrKindPathConcurrency, err)
	info := s.vm.NewObject()
	_ = info.Set("path", err.Path)
	_ = info.Set("open", err.Open)
	_ = info.Set("requested", err.Requests)
	obj, cerr := s.vm.New(s.pathErrCls, s.vm.ToValue(err.Error()), info)
	if cerr != nil {
		return s.vm.NewGoError(err)
	}
	return obj
}

// errRunEnded is the interrupt value of a script that tries to go on after its run ended.
var errRunEnded = errors.New("the run has ended")

// live stops a script that goes on after its run ended (it caught the error that ended it): the VM is interrupted,
// which no try/catch can catch, so the script opens no further position, writes no intent row and starts nothing.
func (s *scriptVM) live() {
	s.e.noteMain()
	if s.e.fatalErr == nil {
		return
	}
	s.vm.Interrupt(errRunEnded)
	panic(s.vm.NewGoError(fmt.Errorf("%w (%s): no further call may open", errRunEnded, s.e.fatalKind)))
}

// open claims the next position of the current path for label; a path with an outstanding position refuses.
func (s *scriptVM) open(label string) (*pathState, int) {
	s.live()
	p := s.cur
	pos, err := p.openPosition(label)
	if err != nil {
		var pe *PathConcurrencyError
		if errors.As(err, &pe) {
			panic(s.pathError(pe))
		}
		panic(s.vm.NewGoError(err))
	}
	return p, pos
}

func (s *scriptVM) openJoin(call goja.FunctionCall) goja.Value {
	label := call.Argument(0).String()
	p, pos := s.open(label)
	j := &joinState{parent: p, position: pos, label: label}
	s.e.j.Append(Event{RunID: s.e.runID, Kind: KindFlow, Path: p.id, Position: pos, Op: "join", Label: label, Event: "join_opened"})
	return s.vm.ToValue(j)
}

func (s *scriptVM) joinArg(v goja.Value) *joinState {
	j, ok := v.Export().(*joinState)
	if !ok {
		s.throwType("not a join")
	}
	return j
}

// runIn runs fn on the child path of a join item: code that runs before fn first awaits, and every continuation after
// it, belongs to that path.
func (s *scriptVM) runIn(call goja.FunctionCall) goja.Value {
	j := s.joinArg(call.Argument(0))
	key := call.Argument(1).String()
	fn, ok := goja.AssertFunction(call.Argument(2))
	if !ok {
		s.throwType("runIn: not a function")
	}
	prev := s.cur
	s.cur = j.parent.child(j.position, key, j)
	defer func() { s.cur = prev }()
	v, err := fn(goja.Undefined())
	if err != nil {
		panic(err)
	}
	return v
}

// closeJoin settles a join: its result is the canonical digest of the item paths' final results ordered by path key (the join
// digest), which every later position of the parent path carries in its input digest. A resumed run replays the join only when
// every position under it replayed and the digest equals the one the previous process journaled; otherwise the positions
// after it on the parent path are the suffix.
func (s *scriptVM) closeJoin(call goja.FunctionCall) goja.Value {
	j := s.joinArg(call.Argument(0))
	digest, children := s.joinDigest(call.Argument(1), call.Argument(2))
	replayed := s.e.joinReplays(j, "join", digest)
	s.e.settleJoin(j.parent, j.position, j.label, "join", digest, children, replayed)
	return goja.Undefined()
}

// joinDigest is the digest of a join's items: each item's key and the canonical JSON of its final result, ordered by key.
func (s *scriptVM) joinDigest(keys, values goja.Value) (string, []map[string]any) {
	ko, kok := keys.(*goja.Object)
	vo, vok := values.(*goja.Object)
	var out []map[string]any
	if kok && vok {
		n := int(ko.Get("length").ToInteger())
		for i := 0; i < n; i++ {
			key := ko.Get(strconv.Itoa(i)).String()
			raw, err := s.jsonOfErr(vo.Get(strconv.Itoa(i)))
			if err != nil {
				raw = json.RawMessage("null")
			}
			out = append(out, map[string]any{"key": key, "digest": canonicalDigest(raw)})
		}
	}
	return joinDigestOf(out), out
}

func (s *scriptVM) itemFailed(call goja.FunctionCall) goja.Value {
	j := s.joinArg(call.Argument(0))
	s.e.j.Append(Event{RunID: s.e.runID, Kind: KindFlow, Path: j.parent.childID(j.position, call.Argument(1).String()), Event: "item_error",
		Data: map[string]any{"error": s.errorText(call.Argument(2))}})
	return goja.Undefined()
}

// textOf renders a thrown value with the String function the runtime started with. The value's own toString runs, so
// the error is its throw or an interrupt.
func (s *scriptVM) textOf(v goja.Value) (string, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "undefined", nil
	}
	out, err := s.toString(goja.Undefined(), v)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// errorText renders a thrown value for a record; a value that cannot be rendered says so.
func (s *scriptVM) errorText(v goja.Value) string {
	t, err := s.textOf(v)
	if err != nil {
		return "(the thrown value cannot be rendered: " + err.Error() + ")"
	}
	return t
}

// jsonOfErr serialises a script value; undefined, and a value JSON has no text for, is null. The value's own toJSON
// and getters run, so the error is their throw (or a cycle) or an interrupt.
func (s *scriptVM) jsonOfErr(v goja.Value) (json.RawMessage, error) {
	if v == nil || goja.IsUndefined(v) {
		return json.RawMessage("null"), nil
	}
	out, err := s.stringify(goja.Undefined(), v)
	if err != nil {
		return nil, err
	}
	if out == nil || goja.IsUndefined(out) {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(out.String()), nil
}

// fromJSON builds a script value from JSON text.
func (s *scriptVM) fromJSON(raw []byte) goja.Value {
	if len(raw) == 0 {
		return goja.Null()
	}
	v, err := s.parse(goja.Undefined(), s.vm.ToValue(string(raw)))
	if err != nil {
		return goja.Null()
	}
	return v
}
