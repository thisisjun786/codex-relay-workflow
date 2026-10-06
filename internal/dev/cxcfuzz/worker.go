//go:build dev

package cxcfuzz

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
// request starts one. A command that is not on PATH is NoCommand.
func NewPool(oracle Oracle, workers int, timeout time.Duration, env []string) (*Pool, error) {
	command, err := exec.LookPath(oracle.Command)
	if err != nil {
		return nil, NoCommand{Command: oracle.Command, Err: err}
	}
	for _, required := range oracle.Requires {
		if _, err := exec.LookPath(required); err != nil {
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
	workerEnv := append([]string{}, env...)
	if oracle.Root != "" {
		workerEnv = append(workerEnv, "ORACLE_ROOT="+oracle.Root)
	}
	pool := &Pool{argv: argv, env: workerEnv, timeout: timeout, slots: make(chan *worker, workers)}
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
	w, err := p.start()
	if err != nil {
		// The slot this call took goes back, or Close would wait for it forever.
		p.slots <- nil
		return nil, err
	}
	return w, nil
}

func (p *Pool) exchange(w *worker, input, root string) (string, error) {
	p.mu.Lock()
	p.seq++
	id := p.seq
	p.mu.Unlock()
	encoded, err := json.Marshal(root)
	if err != nil {
		return "", err
	}
	request := fmt.Sprintf("{\"id\":%d,\"input\":%s,\"root\":%s}\n", id, input, encoded)
	type read struct {
		line string
		err  error
	}
	// The write and the read share one deadline: a worker that stops reading leaves the write
	// blocked once its pipe fills, and only killing the worker unblocks it. Call does that on
	// the Timeout this returns.
	done := make(chan read, 1)
	go func() {
		if _, err := io.WriteString(w.stdin, request); err != nil {
			done <- read{"", err}
			return
		}
		line, err := w.stdout.ReadString('\n')
		done <- read{line, err}
	}()
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case got := <-done:
		if got.err != nil {
			return "", got.err
		}
		return answer(id, got.line)
	case <-timer.C:
		return "", Timeout{}
	}
}

// Close stops every idle worker.
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
