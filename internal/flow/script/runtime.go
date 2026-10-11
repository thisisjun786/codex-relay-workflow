package script

import (
	"errors"
	"fmt"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Limits bound what a script may do between two points where it hands control back to the engine.
//
// A step is one uninterrupted stretch of script execution: from the run's start, or from the engine delivering a
// result, until the script awaits again or ends. Step bounds its wall time (a script that loops without awaiting is
// interrupted), and HeapBytes bounds how far the Go heap may grow while a step runs (sampled; the engine process shares
// the heap, so it is a ceiling against runaway allocation, not an exact account).
type Limits struct {
	Step      time.Duration
	HeapBytes uint64
}

// DefaultLimits are the limits a run has unless its configuration sets others.
var DefaultLimits = Limits{Step: 10 * time.Second, HeapBytes: 512 << 20}

// LimitError is the error a script ends with when a step exceeds a limit.
type LimitError struct {
	Kind   string // "step" or "heap"
	Detail string
}

func (e *LimitError) Error() string { return fmt.Sprintf("%s limit exceeded: %s", e.Kind, e.Detail) }

// stubs make the script deterministic: the wall clock and randomness are not available, and time comes from args or
// the journaled crw.clock(). A Date built from arguments still works.
const stubs = `(function(){
  var NativeDate = Date;
  function DeterminismError(what) {
    var e = new Error(what + ' is not available in a CRW workflow script: it would make the run unrepeatable; read time from args or crw.clock()');
    e.name = 'DeterminismError';
    return e;
  }
  function StubDate() {
    if (!new.target || arguments.length === 0) throw DeterminismError(new.target ? 'new Date()' : 'Date()');
    return Reflect.construct(NativeDate, arguments, new.target === StubDate ? NativeDate : new.target);
  }
  StubDate.prototype = NativeDate.prototype;
  Object.defineProperty(NativeDate.prototype, 'constructor', { value: StubDate, writable: true, configurable: true });
  StubDate.UTC = NativeDate.UTC;
  StubDate.parse = NativeDate.parse;
  StubDate.now = function now() { throw DeterminismError('Date.now()'); };
  globalThis.Date = StubDate;
  Math.random = function random() { throw DeterminismError('Math.random()'); };
})()`

// Loop serialises the completions of asynchronous operations onto one goroutine. Runtimes that share a Loop (a
// script and the workflow it calls) are driven by it one at a time.
type Loop struct {
	mu    sync.Mutex
	queue []queued
	wake  chan struct{}
	// pending counts operations that were started and whose completion is yet to run.
	pending int
	// aborted is set by Abort: no completion runs any more and Run returns.
	aborted bool
}

type queued struct {
	// rt is the runtime fn runs in; nil runs fn outside any script.
	rt *Runtime
	fn func()
	// counted is set when the entry settles an operation that Begin counted as pending.
	counted bool
}

// NewLoop returns an empty loop.
func NewLoop() *Loop { return &Loop{wake: make(chan struct{}, 1)} }

// Runtime is one script's goja runtime. Its VM is touched only by the goroutine that runs its Loop.
type Runtime struct {
	VM     *goja.Runtime
	Loop   *Loop
	limits Limits
	prog   *Program
}

// NewRuntime creates the runtime of one run of p: determinism stubs first, then the helper bundles. No host global
// is defined; the engine adds them with VM.Set before Main.
func (p *Program) NewRuntime(limits Limits) (*Runtime, error) {
	return p.NewRuntimeOn(NewLoop(), limits)
}

// NewRuntimeOn is NewRuntime on a loop shared with other runtimes.
func (p *Program) NewRuntimeOn(loop *Loop, limits Limits) (*Runtime, error) {
	if limits.Step <= 0 {
		limits.Step = DefaultLimits.Step
	}
	if limits.HeapBytes == 0 {
		limits.HeapBytes = DefaultLimits.HeapBytes
	}
	r := &Runtime{VM: goja.New(), Loop: loop, limits: limits, prog: p}
	r.VM.SetMaxCallStackSize(2000)
	if _, err := r.VM.RunString(stubs); err != nil {
		return nil, fmt.Errorf("determinism stubs: %w", err)
	}
	for i, prog := range p.libs {
		if err := r.Enter(func() error { _, err := r.VM.RunProgram(prog); return err }); err != nil {
			return nil, &LoadError{Name: p.Name, Msg: fmt.Sprintf("helper %q: %v", p.Helpers[i].Spec, err)}
		}
		ns, ok := r.VM.Get(p.Helpers[i].GlobalName).(*goja.Object)
		if !ok {
			return nil, &LoadError{Name: p.Name, Msg: fmt.Sprintf("helper %q exports nothing", p.Helpers[i].Spec)}
		}
		for _, name := range p.exports[i] {
			if ns.Get(name) == nil {
				return nil, &LoadError{Name: p.Name, Msg: fmt.Sprintf("helper %q has no export %q", p.Helpers[i].Spec, name)}
			}
		}
	}
	return r, nil
}

// Enter runs fn on the runtime under the step and heap limits. It is the only way into the VM after NewRuntime, and
// must be called from the loop's goroutine.
func (r *Runtime) Enter(fn func() error) error {
	var hit *LimitError
	stop := make(chan struct{})
	finished := make(chan struct{})
	base := heapBytes()
	go func() {
		defer close(finished)
		deadline := time.NewTimer(r.limits.Step)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-deadline.C:
				hit = &LimitError{Kind: "step", Detail: fmt.Sprintf("a step of the script ran longer than %s without awaiting", r.limits.Step)}
			case <-tick.C:
				if cur := heapBytes(); cur > base && cur-base > r.limits.HeapBytes {
					hit = &LimitError{Kind: "heap", Detail: fmt.Sprintf("the heap grew by more than %d bytes during one step", r.limits.HeapBytes)}
				}
			}
			if hit != nil {
				r.VM.Interrupt(hit.Kind + " limit")
				return
			}
		}
	}()
	err := fn()
	close(stop)
	// The watchdog has returned once finished is closed, so hit is settled and no interrupt can arrive later.
	<-finished
	if hit != nil {
		r.VM.ClearInterrupt()
		return hit
	}
	return err
}

func heapBytes() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// Main evaluates the script body and calls it, returning the promise of its completion. The body's return value,
// top-level await and top-level return are the async function's own.
func (r *Runtime) Main() (*goja.Promise, error) {
	var result goja.Value
	err := r.Enter(func() error {
		fn, err := r.VM.RunProgram(r.prog.main)
		if err != nil {
			return err
		}
		call, ok := goja.AssertFunction(fn)
		if !ok {
			return errors.New("the script body did not compile to a function")
		}
		result, err = call(goja.Undefined())
		return err
	})
	if err != nil {
		return nil, err
	}
	p, ok := result.Export().(*goja.Promise)
	if !ok {
		return nil, errors.New("the script body did not return a promise")
	}
	return p, nil
}

// Begin registers an asynchronous operation of rt. The returned function completes it from any goroutine: fn then
// runs on the loop's goroutine, inside rt.Enter. It must be called exactly once. The loop does not return while the
// operation is pending.
func (l *Loop) Begin(rt *Runtime) (complete func(fn func())) {
	l.mu.Lock()
	l.pending++
	l.mu.Unlock()
	return l.completion(rt, true)
}

// Link registers a completion that only the scripts on this loop can produce (a script awaiting the workflow it
// called): it does not keep the loop running. When nothing else is pending and no completion is queued, the script
// that awaits it can never be resumed, and Run returns. Like Begin, the returned function is called exactly once.
func (l *Loop) Link(rt *Runtime) (complete func(fn func())) {
	return l.completion(rt, false)
}

// Post queues fn to run on the loop's goroutine inside rt.Enter (outside any script when rt is nil). It is how code
// that must run in a script, or on the loop goroutine, is handed there from a callback or another goroutine.
func (l *Loop) Post(rt *Runtime, fn func()) {
	l.enqueue(queued{rt: rt, fn: fn})
}

func (l *Loop) completion(rt *Runtime, counted bool) func(fn func()) {
	var once sync.Once
	return func(fn func()) {
		once.Do(func() {
			if !l.enqueue(queued{rt: rt, fn: fn, counted: counted}) && counted {
				l.mu.Lock()
				l.pending--
				l.mu.Unlock()
			}
		})
	}
}

// enqueue adds q to the queue and wakes Run; it reports false when the loop was aborted and q was dropped.
func (l *Loop) enqueue(q queued) bool {
	l.mu.Lock()
	if l.aborted {
		l.mu.Unlock()
		return false
	}
	l.queue = append(l.queue, q)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return true
}

// Abort ends the loop: completions that have not run are dropped, later ones are never queued, and Run returns without
// waiting for the operations still pending (one whose completion would only come from a script that no longer runs
// would keep it waiting for ever). It may be called from any goroutine.
func (l *Loop) Abort() {
	l.mu.Lock()
	l.aborted = true
	l.queue = nil
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Aborted reports whether Abort was called.
func (l *Loop) Aborted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.aborted
}

// Run runs completions on the calling goroutine until no operation is pending or the loop is aborted. A limit error a
// completion hits is passed to onError.
func (l *Loop) Run(onError func(error)) { l.RunObserved(onError, nil) }

// RunObserved is Run that calls after on the loop's goroutine after every completion it ran, so a caller can end the
// run (Abort) as soon as the script has failed instead of after the work still queued.
func (l *Loop) RunObserved(onError func(error), after func()) {
	for {
		l.mu.Lock()
		if l.aborted {
			l.mu.Unlock()
			return
		}
		if len(l.queue) == 0 {
			if l.pending == 0 {
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			<-l.wake
			continue
		}
		q := l.queue[0]
		l.queue = l.queue[1:]
		if q.counted {
			l.pending--
		}
		l.mu.Unlock()
		if q.rt == nil {
			q.fn()
		} else if err := q.rt.Enter(func() error { q.fn(); return nil }); err != nil && onError != nil {
			onError(err)
		}
		if after != nil {
			after()
		}
	}
}
