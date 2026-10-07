//go:build dev

package cxcfuzz

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Timeout is the error of a request the worker did not answer within the pool's deadline. The
// worker is killed and replaced, and the input is recorded as a timeout case.
type Timeout struct{}

func (Timeout) Error() string { return "the oracle worker did not answer in time" }

// NoCommand is the error of an oracle command that is not on PATH. The fuzz command stops on it
// with exit 2 rather than starting a campaign nothing can answer.
type NoCommand struct {
	Command string
	Err     error
}

func (e NoCommand) Error() string {
	return fmt.Sprintf("the oracle worker command %q is not on PATH: %v", e.Command, e.Err)
}

func (e NoCommand) Unwrap() error { return e.Err }

// Pool is a set of long-lived oracle worker processes. Each speaks one NDJSON request per line on
// stdin and one reply per line on stdout, one request in flight per worker.
type Pool struct {
	argv    []string
	env     []string
	timeout time.Duration
	startup time.Duration
	slots   chan *worker
	mu      sync.Mutex
	seq     int
}

type worker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// NewPool resolves the oracle command and prepares the slots. It starts no worker: the first
// request starts one and waits, under startup, until that worker has answered a handshake, so the
// per-request timeout is charged only to a request a ready worker receives. A command that is not
// on PATH is NoCommand.
func NewPool(oracle Oracle, workers int, timeout, startup time.Duration, env []string) (*Pool, error) {
	command, err := exec.LookPath(oracle.Command)
	if err != nil {
		return nil, NoCommand{Command: oracle.Command, Err: err}
	}
	for _, required := range oracle.Requires {
		// Resolve against the environment the worker is launched with, not this process's own PATH: a
		// caller that hands the worker a different PATH must have its requirements judged there.
		if _, err := lookPathIn(env, required); err != nil {
			return nil, NoCommand{Command: required, Err: err}
		}
	}
	argv := []string{command}
	if oracle.Shim != "" {
		argv = append(argv, oracle.Shim)
	}
	if workers < 1 {
		workers = 1
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if startup <= 0 {
		startup = DefaultStartupTimeout
	}
	workerEnv := append([]string{}, env...)
	if oracle.Root != "" {
		workerEnv = append(workerEnv, "ORACLE_ROOT="+oracle.Root)
	}
	pool := &Pool{argv: argv, env: workerEnv, timeout: timeout, startup: startup, slots: make(chan *worker, workers)}
	for i := 0; i < workers; i++ {
		pool.slots <- nil
	}
	return pool, nil
}

func (p *Pool) start() (*worker, error) {
	cmd := exec.Command(p.argv[0], p.argv[1:]...)
	cmd.Env = p.env
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &worker{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

func (w *worker) kill() {
	_ = w.stdin.Close()
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	_ = w.cmd.Wait()
}

// Call sends one request and returns the oracle's answer as JSON text. A reply the oracle marked
// as an error comes back as {"error":{"name":...,"message":...}}, so both sides' failures compare
// as answers; a transport failure (a timeout, a dead worker, an unreadable reply) is an error.
func (p *Pool) Call(input, root string) (string, error) {
	w, err := p.acquire()
	if err != nil {
		return "", err
	}
	output, err := p.exchange(w, input, root)
	if err != nil {
		w.kill()
		p.slots <- nil
		return "", err
	}
	p.slots <- w
	return output, nil
}

func (p *Pool) acquire() (*worker, error) {
	if w := <-p.slots; w != nil {
		return w, nil
	}
	// The startup budget is taken before the process is created, so it covers the whole start --
	// fork/exec and the handshake that proves the worker has booted -- rather than only the part
	// after cmd.Start returns. A start() that alone outran the budget leaves no time for the
	// handshake, and the worker is refused.
	deadline := time.Now().Add(p.startup)
	w, err := p.start()
	if err != nil {
		// The slot this call took goes back, or Close would wait for it forever.
		p.slots <- nil
		return nil, err
	}
	// A freshly started worker is not ready yet: its interpreter is still booting and its shim is
	// still importing its modules. Wait for a handshake reply under the rest of the startup budget,
	// so the per-request timeout below is charged only to an exchange with a ready worker. A worker
	// that never becomes ready is killed and its slot returned, exactly as a failed start.
	if err := p.ready(w, time.Until(deadline)); err != nil {
		w.kill()
		p.slots <- nil
		return nil, err
	}
	return w, nil
}

// nextID is the id of the next request. One sequence serves every worker, so a reply is matched to
// its request even across a worker replacement.
func (p *Pool) nextID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return p.seq
}

// ready waits until a freshly started worker answers one handshake request. The handshake carries
// a null input and an empty root, which every shim answers inertly without reading the homes a case
// declares, so it is a pure readiness probe: a reply exists only after the worker has booted, run
// its top-level imports and started reading its stdin. The reply's content is discarded -- it never
// reaches a target's Compare -- but its envelope is validated, so a worker that answers garbage is
// not treated as ready. An error envelope counts as a reply: the worker is up and answering.
func (p *Pool) ready(w *worker, budget time.Duration) error {
	id := p.nextID()
	request := fmt.Sprintf("{\"id\":%d,\"input\":null,\"root\":\"\"}\n", id)
	line, err := p.roundTrip(w, request, budget)
	if err != nil {
		if errors.Is(err, Timeout{}) {
			return fmt.Errorf("the oracle worker did not become ready in time: %w", err)
		}
		return err
	}
	_, err = answer(id, line)
	return err
}

func (p *Pool) exchange(w *worker, input, root string) (string, error) {
	id := p.nextID()
	encoded, err := json.Marshal(root)
	if err != nil {
		return "", err
	}
	request := fmt.Sprintf("{\"id\":%d,\"input\":%s,\"root\":%s}\n", id, input, encoded)
	line, err := p.roundTrip(w, request, p.timeout)
	if err != nil {
		return "", err
	}
	return answer(id, line)
}

// roundTrip writes one request line and reads one reply line, both inside one deadline: a worker
// that stops reading leaves the write blocked once its pipe fills, and only killing the worker
// unblocks it, so the write and the read share the timer. The reply line is returned unread; the
// caller matches it to its request with answer.
func (p *Pool) roundTrip(w *worker, request string, timeout time.Duration) (string, error) {
	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
		if _, err := io.WriteString(w.stdin, request); err != nil {
			done <- read{"", err}
			return
		}
		line, err := w.stdout.ReadString('\n')
		done <- read{line, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case got := <-done:
		if got.err != nil {
			return "", got.err
		}
		return got.line, nil
	case <-timer.C:
		return "", Timeout{}
	}
}

// lookPathIn resolves a command against an explicit environment's PATH, the way exec.LookPath does
// against this process's own. Only an environment with no PATH entry at all falls back to the process
// PATH, as a child would inherit it; a PATH entry that is present and empty is an empty search path,
// where nothing is found. The two must not be conflated: the worker is launched with this very
// environment, so a check that fell back to the process PATH for an explicitly empty one would accept
// a dependency the worker cannot reach and the campaign would record the worker's failure as a
// fuzzing difference instead of refusing the target before any case runs (CRW-708 generation 5, d5).
func lookPathIn(env []string, command string) (string, error) {
	path, found := "", false
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path, found = value, true
		}
	}
	if !found {
		return exec.LookPath(command)
	}
	if path == "" {
		return "", exec.ErrNotFound
	}
	if strings.ContainsRune(command, filepath.Separator) {
		return command, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, command)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func (p *Pool) Close() error {
	for i := 0; i < cap(p.slots); i++ {
		if w := <-p.slots; w != nil {
			w.kill()
		}
	}
	return nil
}

// answer reads one reply line.
func answer(id int, line string) (string, error) {
	var body struct {
		ID     int             `json:"id"`
		Output json.RawMessage `json:"output"`
		Error  *struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &body); err != nil {
		return "", fmt.Errorf("the worker's reply is not JSON: %w", err)
	}
	if body.ID != id {
		return "", fmt.Errorf("the worker answered request %d as %d", id, body.ID)
	}
	if body.Error != nil {
		name, _ := json.Marshal(body.Error.Name)
		message, _ := json.Marshal(body.Error.Message)
		return fmt.Sprintf("{\"error\":{\"name\":%s,\"message\":%s}}", name, message), nil
	}
	if body.Output == nil {
		return "", errors.New("the worker's reply holds neither an output nor an error")
	}
	return string(body.Output), nil
}
