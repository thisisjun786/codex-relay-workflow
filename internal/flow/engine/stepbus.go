package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the step exchange of a session run (CRW-1316 §3.1-3.2): the run process publishes the steps its session calls
// need, the owner thread takes them (`crw flow next`) and reports each subagent it spawned (`crw flow dispatched`) into the
// run's inbox, and the run answers every report. The run process is the only writer of the journal; the files below are the
// only thing the commands write.
//
// Layout under <state>/flow/<run>/:
//
//	steps/<call>/t<k>-a<n>.json      a step to take (spawn or send_input), until the run binds it or retracts it
//	steps/<call>/close-a<n>.json     a subagent to close (close_agent), until the command that hands it out removes it
//	steps/<call>/packet-t<k>-a<n>.md the task packet of a step (kept: a later attempt or a continuation names it)
//	steps/<call>/<name>              the other files a packet names (the answer a continuation reads)
//	inbox/<id>.json                  a dispatch report, until the run has answered it
//	replies/<id>.json                the run's answer to the report <id>
//
// Every file is written to a temporary name in its directory and renamed, so a reader sees a whole file or none.

// Step actions.
const (
	StepSpawn     = "spawn"
	StepSendInput = "send_input"
	StepClose     = "close"
	StepWait      = "wait"
)

// Schemas of the exchanged files.
const (
	StepSchema           = "crw-flow-step/1"
	DispatchReportSchema = "crw-flow-dispatch/1"
	DispatchReplySchema  = "crw-flow-dispatch-reply/1"
)

// Step is one thing the owner thread is to do for a session call.
type Step struct {
	Schema  string `json:"schema"`
	Run     string `json:"run"`
	Call    string `json:"call"`
	K       int    `json:"k"`
	Attempt int    `json:"attempt"`
	// Op is the operation marker the subagent's turn carries: `<call>/t<k>-a<n>`.
	Op     string `json:"op"`
	Role   string `json:"role"`
	Action string `json:"action"`
	// AgentType, Model and ReasoningEffort are the spawn_agent arguments; Model and ReasoningEffort are empty for a role whose
	// pair the role helper leaves to the host (mode default).
	AgentType       string `json:"agent_type,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Message is the spawn_agent or send_input message; Packet is the absolute path of the task packet it names.
	Message   string `json:"message,omitempty"`
	Packet    string `json:"packet,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// AgentID is the subagent a send_input or close step is for.
	AgentID string `json:"agent_id,omitempty"`
}

// DispatchReport is what `crw flow dispatched` reports: the owner thread spawned (or sent input to) agent for the step.
type DispatchReport struct {
	Schema  string `json:"schema"`
	ID      string `json:"id"`
	Run     string `json:"run"`
	Call    string `json:"call"`
	K       int    `json:"k"`
	Attempt int    `json:"attempt"`
	Agent   string `json:"agent"`
}

// Dispatch reply statuses.
const (
	// ReplyBound: the run bound the agent to the step; nothing more is to be done for it.
	ReplyBound = "bound"
	// ReplyClose: the step the report names has ended (an attempt that was answered, lost or superseded): close the agent.
	ReplyClose = "close"
	// ReplyPairMismatch: the agent runs another pair than the role's: close it and spawn again with the pair the reply names;
	// the step stays.
	ReplyPairMismatch = "pair_mismatch"
	// ReplyRetry: the run could not read what it must check yet (the host, the marker turn): report again.
	ReplyRetry = "retry"
	// ReplyRefused: the report names no step of the run, or an agent that is not the owner thread's subagent; the step stays.
	ReplyRefused = "refused"
)

// DispatchReply is the run's answer to a dispatch report.
type DispatchReply struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Close is the agent to close (close, pair_mismatch).
	Close string `json:"close,omitempty"`
}

// StepBus is how a session run exchanges steps and dispatch reports with the commands the owner thread runs.
type StepBus interface {
	// FilePath is the absolute path a file of a call's step directory has (the path a step row names before the file is written).
	FilePath(call, name string) string
	// WriteFile writes a file of a call's step directory (a packet, the answer a continuation reads) and returns its absolute path.
	WriteFile(call, name string, body []byte) (string, error)
	// Publish writes a step.
	Publish(step Step) error
	// Retract removes a step that is no longer to be taken (it was bound, or its call ended).
	Retract(call string, k, attempt int) error
	// Reports returns the dispatch reports the run has not answered, in the order of their ids.
	Reports() ([]DispatchReport, error)
	// Reply answers a report and removes it from the inbox.
	Reply(rep DispatchReport, reply DispatchReply) error
}

// FileStepBus is the StepBus over the run's directory <state>/flow/<run>.
type FileStepBus struct {
	// Dir is the run's directory.
	Dir string
}

// NewFileStepBus is the bus of a run's directory.
func NewFileStepBus(dir string) *FileStepBus { return &FileStepBus{Dir: dir} }

// StepsDir is the directory of a call's steps.
func (b *FileStepBus) StepsDir(call string) string {
	return filepath.Join(b.Dir, "steps", pathElem(call))
}

// InboxDir is the directory dispatch reports are written to.
func (b *FileStepBus) InboxDir() string { return filepath.Join(b.Dir, "inbox") }

// RepliesDir is the directory the run's answers are written to.
func (b *FileStepBus) RepliesDir() string { return filepath.Join(b.Dir, "replies") }

// StepName is the file name of a step to take.
func StepName(k, attempt int) string { return fmt.Sprintf("t%d-a%d.json", k, attempt) }

// PacketName is the file name of a step's packet.
func PacketName(k, attempt int) string { return fmt.Sprintf("packet-t%d-a%d.md", k, attempt) }

// CloseName is the file name of a close step.
func CloseName(attempt int) string { return fmt.Sprintf("close-a%d.json", attempt) }

// pathElem makes an id one path element.
func pathElem(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, s)
	if s == "" || s == "." || s == ".." {
		return "_" + s
	}
	return s
}

// WriteAtomic writes a file through a temporary name in its directory and a rename.
func WriteAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// FilePath implements StepBus.
func (b *FileStepBus) FilePath(call, name string) string {
	return filepath.Join(b.StepsDir(call), pathElem(name))
}

// WriteFile implements StepBus.
func (b *FileStepBus) WriteFile(call, name string, body []byte) (string, error) {
	path := b.FilePath(call, name)
	if err := WriteAtomic(path, body); err != nil {
		return "", err
	}
	return path, nil
}

// Publish implements StepBus.
func (b *FileStepBus) Publish(step Step) error {
	step.Schema = StepSchema
	raw, err := json.MarshalIndent(step, "", "  ")
	if err != nil {
		return err
	}
	name := StepName(step.K, step.Attempt)
	if step.Action == StepClose {
		name = CloseName(step.Attempt)
	}
	return WriteAtomic(filepath.Join(b.StepsDir(step.Call), name), append(raw, '\n'))
}

// Retract implements StepBus.
func (b *FileStepBus) Retract(call string, k, attempt int) error {
	err := os.Remove(filepath.Join(b.StepsDir(call), StepName(k, attempt)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Steps reads every step file of the run (the steps to take and the agents to close), in call and file order.
func (b *FileStepBus) Steps() ([]Step, error) {
	calls, err := os.ReadDir(filepath.Join(b.Dir, "steps"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Step
	for _, c := range calls {
		if !c.IsDir() {
			continue
		}
		dir := filepath.Join(b.Dir, "steps", c.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			n := f.Name()
			if !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") || !(strings.HasPrefix(n, "t") || strings.HasPrefix(n, "close-")) {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, err
			}
			var s Step
			if err := json.Unmarshal(raw, &s); err != nil || s.Schema != StepSchema {
				continue
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// Report writes a dispatch report into the inbox (what `crw flow dispatched` does).
func (b *FileStepBus) Report(rep DispatchReport) error {
	if rep.ID == "" {
		return errors.New("engine: a dispatch report needs an id")
	}
	rep.Schema = DispatchReportSchema
	raw, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(b.InboxDir(), pathElem(rep.ID)+".json"), append(raw, '\n'))
}

// ReadReply reads the run's answer to a report, found false while there is none.
func (b *FileStepBus) ReadReply(id string) (DispatchReply, bool, error) {
	raw, err := os.ReadFile(filepath.Join(b.RepliesDir(), pathElem(id)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return DispatchReply{}, false, nil
	}
	if err != nil {
		return DispatchReply{}, false, err
	}
	var r DispatchReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return DispatchReply{}, false, err
	}
	return r, true, nil
}

// Reports implements StepBus.
func (b *FileStepBus) Reports() ([]DispatchReport, error) {
	files, err := os.ReadDir(b.InboxDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []DispatchReport
	for _, f := range files {
		n := f.Name()
		if f.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(b.InboxDir(), n))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var r DispatchReport
		if err := json.Unmarshal(raw, &r); err != nil || r.Schema != DispatchReportSchema || r.ID == "" {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

// Reply implements StepBus: the answer is written first, so a run that stops in between answers the report again (the same
// way) and the command never waits on a report that was taken away.
func (b *FileStepBus) Reply(rep DispatchReport, reply DispatchReply) error {
	reply.Schema, reply.ID = DispatchReplySchema, rep.ID
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	if err := WriteAtomic(filepath.Join(b.RepliesDir(), pathElem(rep.ID)+".json"), append(raw, '\n')); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(b.InboxDir(), pathElem(rep.ID)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
