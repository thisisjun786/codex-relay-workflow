//go:build dev

package cxcfuzz

import (
	"bufio"
	"encoding/json"
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
	return p.start()
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
	if _, err := io.WriteString(w.stdin, request); err != nil {
		return "", err
	}
	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
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
		return answer(got.line)
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
func answer(line string) (string, error) {
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
	if body.Error != nil {
		name, _ := json.Marshal(body.Error.Name)
		message, _ := json.Marshal(body.Error.Message)
		return fmt.Sprintf("{\"error\":{\"name\":%s,\"message\":%s}}", name, message), nil
	}
	if len(body.Output) == 0 {
		return "null", nil
	}
	return string(body.Output), nil
}
