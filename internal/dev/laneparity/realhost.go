//go:build dev

package laneparity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The real-host cells (CRW-1082): the real Codex binary runs whole turns in an isolated home against
// a stub model provider (StubProvider), with the plugin that ships placed in the home's plugin cache
// and trusted by `crw doctor retrust` (the built crw, in that home only). The hooks the host starts
// are the declared commands, verbatim: the installed runtime path they name holds a recorder that
// keeps the arguments, the payload on stdin, the exit status and the answer of every start and hands
// the start to the build under test (hostShim). A cell passes only when the hooks the host started
// are exactly the declared ones whose event and matcher the turn's events match, for the session the
// host reported, with the answers the switch allows. Nothing here uses --dangerously-bypass-hook-trust
// or any other flag that skips trust, and nothing reads or writes the real Codex home, the installed
// runtime, auth files or services: the provider is a loopback server and the home is a temporary one.

// The cells of a real-host run.
const (
	CellTurnCRW      = "turn/crw"       // switch at crw: the ported legs run and their answers reach the model
	CellTurnOff      = "turn/off"       // no switch file: the host starts every declared hook, the ported legs are silent
	CellTurnCXC      = "turn/cxc"       // switch at cxc: the same
	CellUntrusted    = "untrusted/crw"  // no trust recorded: the host starts none of them
	CellCompaction   = "compaction/crw" // a real compaction: PostCompact legs fire and the recall directive reaches the model
	CellPermission   = "permission/crw" // an escalation the host must ask permission for
	CellSpawn        = "spawn/crw"      // a spawned agent that ends: SubagentStop
	hostTurnPrompt   = "Run the crw real-host check."
	hostMarket       = "crwtest"
	hostCompactLimit = 100 // auto-compaction limit (tokens) of the compaction cell; the stub reports more
)

// RealHostOptions is one real-host run.
type RealHostOptions struct {
	Root    string        // the repository
	CRW     string        // the crw build under test (absolute)
	Plugin  string        // the plugin root placed in the home's plugin cache (the shipped command form)
	Codex   string        // the Codex executable; empty means codex on PATH
	Scratch string        // parent of the run's homes
	Timeout time.Duration // per Codex run; 90 s when zero
	// Only restricts the cells run to those whose name matches (nil: all).
	Only *regexp.Regexp
	// Fault changes the recorder: FaultNoop starts the build for no hook (every hook answers nothing),
	// FaultDropStdout runs the build but hands the host nothing of its answer. The run must fail.
	Fault string
}

// HostCodex names the Codex the cells ran.
type HostCodex struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// HostFiring is one start of a declared hook by the host, as the recorder saw it.
type HostFiring struct {
	Leg       string `json:"leg"`
	Event     string `json:"event"` // hook_event_name of the payload
	ToolName  string `json:"toolName,omitempty"`
	ToolUse   string `json:"toolUse,omitempty"`
	Session   string `json:"session"`
	Turn      string `json:"turn,omitempty"`
	Source    string `json:"source,omitempty"` // why a session started: startup, compact ...
	Exit      int    `json:"exit"`
	Answered  bool   `json:"answered"`
	StdoutSHA string `json:"stdoutSha256"`
	// Context is whether the answer carries text for the model (additionalContext, or plain text), and
	// Reached whether that text was in a request the model received.
	Context bool `json:"context,omitempty"`
	Reached bool `json:"reachedModel,omitempty"`
}

// HostCell is the outcome of one real Codex run.
type HostCell struct {
	Name    string `json:"name"`
	Switch  string `json:"switch"`
	Trusted bool   `json:"trusted"`
	Thread  string `json:"thread,omitempty"`
	Exit    int    `json:"codexExit"`
	// Trust is what `crw doctor retrust` reported for the home (empty for an untrusted cell).
	Trust string `json:"trust,omitempty"`
	// Events counts the starts by event name; Records are the invocation records the home held.
	Events       map[string]int `json:"events"`
	Firings      []HostFiring   `json:"firings"`
	Records      int            `json:"observationRecords"`
	ModelRequest int            `json:"modelRequests"`
	Answered     int            `json:"answered"`
	Reached      int            `json:"answersReachedModel"`
	Problems     []string       `json:"problems,omitempty"`
	// Unverified is set for a cell the host could not be driven into: why, with what was measured.
	Unverified string `json:"unverified,omitempty"`
	OK         bool   `json:"ok"`
}

// RealHostReport is the real-host part of a run.
type RealHostReport struct {
	Run    string    `json:"run"`
	Codex  HostCodex `json:"codex"`
	Plugin string    `json:"pluginDigest"`
	Binary string    `json:"binarySha256"`
	// Bypass is whether any run used a flag that skips hook trust. It is false by construction; the
	// field is in the report so a reader does not have to take that on trust.
	Bypass bool `json:"bypassHookTrust"`
	// Args are the arguments of the host's turns (the work directory and prompt are the same in every
	// cell), as the report's reader checks them for a flag that skips trust, approvals or the sandbox.
	Args  []string   `json:"codexArgs,omitempty"`
	Cells []HostCell `json:"cells"`
	// Skipped is why no cell ran (the Codex binary is not there), and Unverified the cells that did
	// not verify, each with the reason.
	Skipped    string        `json:"skipped,omitempty"`
	Unverified []NotVerified `json:"unverified,omitempty"`
	OK         bool          `json:"ok"`
}

// hostCellSpec is how one cell is run and judged.
type hostCellSpec struct {
	name    string
	state   string // SwitchOn, SwitchOff or SwitchCXC
	trusted bool
	config  string // lines for the top of config.toml
	script  func(*hostScript) func(StubRequest) StubReply
	// manyTurns is whether the cell's hooks belong to more than one turn (an agent the turn spawns
	// has a turn of its own).
	manyTurns bool
	// events are the host events the script causes, for the exact set of hooks the host must start.
	events []hostEvent
	// check adds the cell's own judgement.
	check func(*HostCell, *hostRun)
}

// hostEvent is one occurrence of a hook event: the event, and the name its matchers are tried
// against (the tool of a tool event).
type hostEvent struct{ Event, Tool string }

// hostScript carries what a script needs: the image the view_image call opens.
type hostScript struct{ image string }

// RealHost runs the real-host cells.
func RealHost(o RealHostOptions) (RealHostReport, error) {
	rep := RealHostReport{Run: NewRunID(), OK: true}
	var err error
	if rep.Binary, err = FileDigest(o.CRW); err != nil {
		return rep, err
	}
	if rep.Plugin, err = PluginDigest(o.Plugin); err != nil {
		return rep, err
	}
	manifest, registered, err := ReadRegistered(o.Plugin)
	if err != nil {
		return rep, err
	}
	for _, r := range registered {
		if word, ok := firstWord(r.Command); !ok || word != "$HOME/"+runtimeBin {
			return rep, fmt.Errorf("%s declares %q: a real-host run needs the plugin as it ships, starting \"$HOME/%s\"", r.File, r.Command, runtimeBin)
		}
	}
	codex, reason := findCodex(o.Codex)
	if codex == "" {
		rep.Skipped = reason
		for _, name := range hostCellNames {
			rep.Unverified = append(rep.Unverified, NotVerified{"real host: " + name, reason, "run where the Codex binary is on PATH (or pass --codex)"})
		}
		return rep, nil
	}
	if o.Timeout == 0 {
		o.Timeout = 90 * time.Second
	}
	if o.Fault != FaultNone && o.Fault != FaultNoop && o.Fault != FaultDropStdout {
		return rep, fmt.Errorf("fault %q cannot be injected into a real-host run (choose %s or %s)", o.Fault, FaultNoop, FaultDropStdout)
	}
	scratch, err := os.MkdirTemp(o.Scratch, "rh-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(scratch)
	if rep.Codex, err = describeCodex(codex, scratch); err != nil {
		return rep, err
	}
	rep.Args = hostArgs("<work>")
	h := &hostEnv{opts: o, codex: codex, scratch: scratch, manifest: manifest, registered: registered}
	for _, spec := range hostCellSpecs() {
		if o.Only != nil && !o.Only.MatchString(spec.name) {
			continue
		}
		cell, err := h.run(spec)
		if err != nil {
			return rep, fmt.Errorf("%s: %w", spec.name, err)
		}
		rep.Cells = append(rep.Cells, cell)
		rep.OK = rep.OK && cell.OK
		if cell.Unverified != "" {
			rep.Unverified = append(rep.Unverified, NotVerified{"real host: " + spec.name, cell.Unverified, "a host turn that asks for permission (Codex exec runs with approval never)"})
		}
	}
	return rep, nil
}

// hostCellNames are the cells in the order they run.
var hostCellNames = []string{CellTurnCRW, CellTurnOff, CellTurnCXC, CellUntrusted, CellCompaction, CellPermission, CellSpawn}

// findCodex is the Codex executable: the one named, else codex on PATH. The reason is set when there
// is none.
func findCodex(named string) (string, string) {
	if named != "" {
		abs, err := filepath.Abs(named)
		if err != nil {
			return "", err.Error()
		}
		if info, err := os.Stat(abs); err != nil || info.IsDir() {
			return "", "the Codex executable " + named + " is not there"
		}
		return abs, ""
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", "no codex executable on PATH"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err.Error()
	}
	return abs, ""
}

// describeCodex names the Codex executable and the version it reports. The version is asked for in
// a home of its own (a directory of the run), never the caller's.
func describeCodex(path, home string) (HostCodex, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return HostCodex{}, err
	}
	digest, err := FileDigest(real)
	if err != nil {
		return HostCodex{}, err
	}
	cmd := exec.Command(path, "--version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "CODEX_HOME=" + filepath.Join(home, "codex-version")}
	out, err := cmd.Output()
	if err != nil {
		return HostCodex{}, fmt.Errorf("codex --version: %w", err)
	}
	return HostCodex{Path: real, Version: strings.TrimSpace(string(out)), SHA256: digest}, nil
}

// hostEnv is what the cells of one run share.
type hostEnv struct {
	opts       RealHostOptions
	codex      string
	scratch    string
	manifest   Manifest
	registered []Registered
	n          int
}

// hostRun is a cell's run as the judgement sees it.
type hostRun struct {
	codexHome string
	requests  []StubRequest
	output    string // the host's JSON event stream
	stderr    string
	items     []hostItem
}

// hostItem is an item the host reported on its JSON stream.
type hostItem struct {
	Type    string
	Command string
	Status  string
	Text    string
}

func (h *hostEnv) run(spec hostCellSpec) (HostCell, error) {
	cell := HostCell{Name: spec.name, Switch: spec.state, Trusted: spec.trusted, Events: map[string]int{}}
	h.n++
	dir := filepath.Join(h.scratch, fmt.Sprintf("c%d", h.n))
	r := &hostRun{codexHome: filepath.Join(dir, "codex")}
	home, work, tmp, rec := filepath.Join(dir, "home"), filepath.Join(dir, "work"), filepath.Join(dir, "tmp"), filepath.Join(dir, "rec")
	for _, d := range []string{home, r.codexHome, work, tmp, rec} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return cell, err
		}
	}
	// The installed runtime the declarations start is the recorder.
	shim := filepath.Join(home, runtimeBin)
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		return cell, err
	}
	if err := os.WriteFile(shim, []byte(hostShim(h.opts.CRW, rec, h.opts.Fault)), 0o755); err != nil {
		return cell, err
	}
	// The plugin as the host's plugin cache holds it: <cache>/<marketplace>/<plugin>/<version>.
	version, err := manifestVersion(h.opts.Plugin)
	if err != nil {
		return cell, err
	}
	if err := copyPlugin(h.opts.Plugin, filepath.Join(r.codexHome, "plugins", "cache", hostMarket, h.manifest.Name, version)); err != nil {
		return cell, err
	}
	script := &hostScript{image: filepath.Join(work, "image.png")}
	if err := writePNG(script.image); err != nil {
		return cell, err
	}
	provider, err := StartStubProvider(spec.script(script))
	if err != nil {
		return cell, err
	}
	defer provider.Close()
	config := spec.config + fmt.Sprintf(`model = "stub-model"
model_provider = "stub"
approval_policy = "never"
sandbox_mode = "read-only"

[model_providers.stub]
name = "stub"
base_url = %q
wire_api = "responses"
requires_openai_auth = false

[plugins.%q]
enabled = true
`, provider.URL(), h.manifest.Name+"@"+hostMarket)
	if err := os.WriteFile(filepath.Join(r.codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		return cell, err
	}
	if spec.state != SwitchOff {
		state := spec.state
		doc := fmt.Sprintf("{\"active\":%q,\"changedAt\":%q,\"by\":%q}\n", state, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), SwitchBy)
		if err := os.MkdirAll(filepath.Join(r.codexHome, "crw"), 0o700); err != nil {
			return cell, err
		}
		if err := os.WriteFile(filepath.Join(r.codexHome, "crw", "switch.json"), []byte(doc), 0o644); err != nil {
			return cell, err
		}
	}
	env := hostEnviron(home, r.codexHome, tmp, h.codex)
	if spec.trusted {
		out, code := runHostCommand(h.opts.Timeout, dir, env, nil, h.opts.CRW, "doctor", "retrust", "--bootstrap-ok")
		cell.Trust = trustSummary(out)
		if code != 0 {
			cell.Problems = append(cell.Problems, fmt.Sprintf("crw doctor retrust exited %d: %s", code, firstLines(out, 3)))
			return cell, nil
		}
		if want := fmt.Sprintf("appended=%d", len(h.registered)); !strings.Contains(out, want) {
			cell.Problems = append(cell.Problems, fmt.Sprintf("crw doctor retrust trusted %q, want %s (every declared hook)", cell.Trust, want))
		}
	}
	// The turn. Trust comes from the config retrust wrote, never from a flag.
	out, code := runHostCommand(h.opts.Timeout, work, env, &r.stderr, h.codex, hostArgs(work)...)
	cell.Exit = code
	r.output = out
	r.requests = provider.Requests()
	cell.ModelRequest = len(r.requests)
	cell.Thread, r.items = parseHostStream(out)
	if err := h.collect(&cell, r, rec); err != nil {
		return cell, err
	}
	h.judge(&cell, r, spec)
	cell.OK = len(cell.Problems) == 0 // a cell the host could not be driven into is Unverified, not failed
	return cell, nil
}

// hostArgs are the arguments of a turn: no flag skips trust, approvals or the sandbox; the hooks the
// host starts are the ones the config.toml of the home trusts.
func hostArgs(work string) []string {
	return []string{"exec", "--json", "--skip-git-repo-check", "-C", work, hostTurnPrompt}
}

// hostShim is the recorder installed as the runtime: it keeps the arguments, the payload, the
// answer, the exit status and the time of every start in its own directory under rec, then hands the
// start to the build under test with the same arguments and payload and returns that build's answer
// and status unchanged.
func hostShim(crw, rec, fault string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	run := q(crw) + " \"$@\" < \"$d/in\" > \"$d/out\"\nrc=$?\n"
	cat := "cat \"$d/out\"\n"
	switch fault {
	case FaultNoop:
		run = ": > \"$d/out\"\nrc=0\n"
	case FaultDropStdout:
		cat = ""
	}
	return "#!/bin/sh\nPATH=/usr/bin:/bin\nexport PATH\nd=$(mktemp -d " + q(rec+"/r.XXXXXX") + ") || exit 70\n" +
		"date +%s%N > \"$d/at\"\ncat > \"$d/in\"\nprintf '%s\\n' \"$@\" > \"$d/argv\"\n" +
		run + "printf '%s\\n' \"$rc\" > \"$d/rc\"\n" + cat + "exit $rc\n"
}

// hostEnviron is the environment of every process of a cell: nothing of this user's, a PATH of the
// system programs, the temporary home, and a proxy that refuses every connection but the loopback
// provider (the host tries to reach the network for its plugin catalogue otherwise).
func hostEnviron(home, codexHome, tmp, codex string) []string {
	dead := "http://127.0.0.1:9"
	return []string{"PATH=/usr/bin:/bin", "HOME=" + home, "CODEX_HOME=" + codexHome, "TMPDIR=" + tmp, "CODEX_BIN=" + codex,
		"HTTP_PROXY=" + dead, "HTTPS_PROXY=" + dead, "ALL_PROXY=" + dead, "http_proxy=" + dead, "https_proxy=" + dead, "all_proxy=" + dead,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

// runHostCommand runs a process to its end in its own group (killed with it) with a closed stdin and
// returns its stdout and exit status (-1 when it was killed or did not start). stderr, when asked
// for, is kept apart.
func runHostCommand(timeout time.Duration, dir string, env []string, stderr *string, file string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, file, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	if stderr != nil {
		cmd.Stderr = &errb
	} else {
		cmd.Stderr = &out
	}
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever a hook left running dies with the turn
	}
	if stderr != nil {
		*stderr = errb.String()
	}
	code := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		code = -1
	}
	return out.String(), code
}

// trustSummary is the "updated=N appended=M" line of crw doctor retrust.
func trustSummary(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "updated=") {
			return strings.TrimSpace(line)
		}
	}
	return firstLines(out, 1)
}

// parseHostStream reads the JSON event stream of `codex exec --json`: the thread id and the items.
func parseHostStream(out string) (string, []hostItem) {
	var thread string
	var items []hostItem
	for _, line := range strings.Split(out, "\n") {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Status  string `json:"status"`
				Text    string `json:"text"`
				Message string `json:"message"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			thread = ev.ThreadID
		case "item.completed":
			items = append(items, hostItem{Type: ev.Item.Type, Command: ev.Item.Command, Status: ev.Item.Status, Text: ev.Item.Text + ev.Item.Message})
		case "turn.completed":
			items = append(items, hostItem{Type: "turn.completed"})
		}
	}
	return thread, items
}

// manifestVersion is the version a plugin root's manifest names, the directory a plugin cache keeps
// the package in.
func manifestVersion(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ".codex-plugin", "plugin.json"))
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Version == "" {
		return "", fmt.Errorf("the plugin manifest names no version: %v", err)
	}
	return m.Version, nil
}

// copyPlugin copies a plugin root into the plugin cache, following links, as an installation holds
// the package as plain files. A link to a directory is copied as that directory; a link that leads
// back into a directory being copied (a cycle) is an error, not an endless copy.
func copyPlugin(from, to string) error {
	return copyTree(from, to, map[string]bool{})
}

// copyTree copies src (a file, a directory, or a link to either) to dst. active holds the real paths
// of the directories being copied on the way down to src.
func copyTree(src, dst string, active map[string]bool) error {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		raw, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, info.Mode().Perm())
	}
	if active[real] {
		return fmt.Errorf("%s links back to %s, a directory being copied: a plugin package holds no link cycle", src, real)
	}
	active[real] = true
	defer delete(active, real)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), active); err != nil {
			return err
		}
	}
	return nil
}

// writePNG writes the 2x2 image the script's view_image call opens.
func writePNG(path string) error {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// payload is the part of a hook payload the cells read.
type payload struct {
	Event    string `json:"hook_event_name"`
	Session  string `json:"session_id"`
	Turn     string `json:"turn_id"`
	ToolName string `json:"tool_name"`
	ToolUse  string `json:"tool_use_id"`
	Source   string `json:"source"`
}

// collect reads what the recorder and the host's invocation records kept into the cell.
func (h *hostEnv) collect(cell *HostCell, r *hostRun, rec string) error {
	dirs, err := filepath.Glob(filepath.Join(rec, "r.*"))
	if err != nil {
		return err
	}
	type started struct {
		at int64
		f  HostFiring
	}
	var all []started
	for _, d := range dirs {
		argv, errA := os.ReadFile(filepath.Join(d, "argv"))
		in, errI := os.ReadFile(filepath.Join(d, "in"))
		out, errO := os.ReadFile(filepath.Join(d, "out"))
		rc, errR := os.ReadFile(filepath.Join(d, "rc"))
		at, _ := os.ReadFile(filepath.Join(d, "at"))
		if errA != nil || errI != nil || errO != nil || errR != nil {
			cell.Problems = append(cell.Problems, fmt.Sprintf("a hook start left an incomplete record (%s)", filepath.Base(d)))
			continue
		}
		f := HostFiring{Leg: legOfArgv(strings.Fields(string(argv)))}
		var p payload
		if json.Unmarshal(in, &p) != nil {
			cell.Problems = append(cell.Problems, fmt.Sprintf("%s: the payload on stdin is not JSON", f.Leg))
		}
		f.Event, f.ToolName, f.ToolUse, f.Session, f.Turn, f.Source = p.Event, p.ToolName, p.ToolUse, p.Session, p.Turn, p.Source
		fmt.Sscan(strings.TrimSpace(string(rc)), &f.Exit)
		f.Answered = len(bytes.TrimSpace(out)) > 0
		f.StdoutSHA = StdoutDigest(string(out))
		var ns int64
		fmt.Sscan(strings.TrimSpace(string(at)), &ns)
		all = append(all, started{ns, f})
		if text := answerText(string(out)); f.Answered && text != "" {
			all[len(all)-1].f.Context = true
			all[len(all)-1].f.Reached = requestsHold(r.requests, text)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at < all[j].at })
	for _, s := range all {
		cell.Firings = append(cell.Firings, s.f)
		cell.Events[s.f.Event]++
		if s.f.Answered {
			cell.Answered++
		}
		if s.f.Reached {
			cell.Reached++
		}
	}
	records, _ := filepath.Glob(filepath.Join(r.codexHome, "crw", "hook-observations", "*", "*", "*.json"))
	cell.Records = len(records)
	for _, path := range records {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec struct {
			Session string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &rec) != nil || (cell.Thread != "" && rec.Session != cell.Thread) {
			cell.Problems = append(cell.Problems, fmt.Sprintf("an invocation record names session %q, the host's thread is %q", rec.Session, cell.Thread))
		}
	}
	return nil
}

// legOfArgv is the leg a start of the runtime was for: --leg <leg>, or the completion Stop's
// `hook --plugin-launch`.
func legOfArgv(argv []string) string {
	for i, a := range argv {
		if a == "--leg" && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--leg="); ok {
			return v
		}
		if a == "--plugin-launch" {
			return CompletionLeg
		}
	}
	return "?"
}

// answerText is the context a hook answers with: the additionalContext of a JSON answer, or the whole
// text of a plain one.
func answerText(stdout string) string {
	s := strings.TrimSpace(stdout)
	var doc struct {
		Specific struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if strings.HasPrefix(s, "{") && json.Unmarshal([]byte(s), &doc) == nil {
		return strings.TrimSpace(doc.Specific.Context)
	}
	if strings.HasPrefix(s, "{") {
		return ""
	}
	return s
}

// requestsHold is whether a model request carried text.
func requestsHold(requests []StubRequest, text string) bool {
	for _, r := range requests {
		for _, t := range r.Texts {
			if strings.Contains(t, text) {
				return true
			}
		}
	}
	return false
}

// judge adds the problems of the cell: the hooks the host started against the hooks that must start,
// then what is particular to the cell's switch and script.
func (h *hostEnv) judge(cell *HostCell, r *hostRun, spec hostCellSpec) {
	add := func(format string, args ...any) { cell.Problems = append(cell.Problems, fmt.Sprintf(format, args...)) }
	if cell.Exit != 0 {
		add("codex exec exited %d: %s %s", cell.Exit, firstLines(r.output, 3), firstLines(r.stderr, 3))
	}
	if cell.Thread == "" {
		add("the host reported no thread")
	}
	if !spec.trusted {
		if len(cell.Firings) > 0 {
			add("the host started %d hook(s) with no trust recorded", len(cell.Firings))
		}
	} else {
		wantKeys := dueHooks(h.registered, spec.events)
		gotKeys := map[string]int{}
		for _, f := range cell.Firings {
			gotKeys[hookKey(f.Leg, f.Event, f.ToolName)]++
		}
		for _, k := range sortedKeysOf(wantKeys) {
			if gotKeys[k] != wantKeys[k] {
				add("hook %s started %d time(s), want %d", k, gotKeys[k], wantKeys[k])
			}
		}
		for _, k := range sortedKeysOf(gotKeys) {
			if wantKeys[k] == 0 {
				add("hook %s started %d time(s), no declared hook is due for it", k, gotKeys[k])
			}
		}
		turn := ""
		for _, f := range cell.Firings {
			if f.Session != cell.Thread {
				add("%s (%s) was given session %q, the host's thread is %q", f.Leg, f.Event, f.Session, cell.Thread)
			}
			if f.Exit != 0 {
				add("%s (%s) exited %d", f.Leg, f.Event, f.Exit)
			}
			if f.Event == "PreToolUse" || f.Event == "PostToolUse" {
				if f.ToolUse == "" {
					add("%s (%s) was given no tool call id", f.Leg, f.Event)
				}
			}
			if !spec.manyTurns && (f.Event == "UserPromptSubmit" || f.Event == "PreToolUse" || f.Event == "PostToolUse" || f.Event == "Stop") {
				if f.Turn == "" {
					add("%s (%s) was given no turn id", f.Leg, f.Event)
				} else if turn == "" {
					turn = f.Turn
				} else if f.Turn != turn {
					add("%s (%s) was given turn %q, an earlier hook of the turn got %q", f.Leg, f.Event, f.Turn, turn)
				}
			}
		}
	}
	if spec.check != nil {
		spec.check(cell, r)
	}
}

// hookKey names a start for the exact comparison: the leg, the event and, for a tool event, the tool
// (the subject of any other event's matcher, a role, does not take part).
// dueHooks counts, by hookKey, the starts the declared hooks are due for when the host sees the
// events: every registration whose event is one of them and whose matcher takes the event's subject.
func dueHooks(registered []Registered, events []hostEvent) map[string]int {
	due := map[string]int{}
	for _, ev := range events {
		for _, reg := range registered {
			if reg.Event == ev.Event && matcherMatches(reg.Matcher, ev.Tool) {
				leg := reg.Leg
				if leg == "" {
					leg = CompletionLeg
				}
				due[hookKey(leg, ev.Event, ev.Tool)]++
			}
		}
	}
	return due
}

func hookKey(leg, event, tool string) string {
	if tool == "" || (event != "PreToolUse" && event != "PostToolUse") {
		return leg + "|" + event
	}
	return leg + "|" + event + "|" + tool
}

func sortedKeysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// matcherMatches is whether a hook declared with matcher runs for a name: no matcher and * run for
// every name, any other matcher is a regular expression tried against it.
func matcherMatches(matcher, name string) bool {
	if matcher == "" || matcher == "*" {
		return true
	}
	re, err := regexp.Compile(matcher)
	return err == nil && re.MatchString(name)
}

// ---- the scripts and cells

// turnScript is a turn with two tool calls and an answer: a shell command (the tool the host names
// Bash to its hooks) and the image viewer.
func turnScript(s *hostScript) func(StubRequest) StubReply {
	return func(r StubRequest) StubReply {
		switch r.Outputs {
		case 0:
			return StubReply{Call: &StubCall{Name: "exec_command", Arguments: `{"cmd":"echo crw-real-host"}`}}
		case 1:
			args, _ := json.Marshal(map[string]string{"path": s.image})
			return StubReply{Call: &StubCall{Name: "view_image", Arguments: string(args)}}
		}
		return StubReply{Text: "crw real-host turn done"}
	}
}

// compactionScript makes the host compact its context: the first request is answered with a tool call
// that reports more usage than the cell's auto-compaction limit, the compaction request with a
// summary, and the request after it with the answer.
func compactionScript(*hostScript) func(StubRequest) StubReply {
	var mu sync.Mutex
	compacted := false
	return func(r StubRequest) StubReply {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Compaction:
			compacted = true
			return StubReply{Text: "summary of the crw real-host turn"}
		case compacted:
			return StubReply{Text: "crw real-host turn done after the compaction"}
		}
		return StubReply{Call: &StubCall{Name: "exec_command", Arguments: `{"cmd":"echo crw-real-host"}`}, Tokens: hostCompactLimit * 50}
	}
}

// permissionScript asks for a command outside the sandbox, which a host that can ask for permission
// asks before it runs.
func permissionScript(*hostScript) func(StubRequest) StubReply {
	return func(r StubRequest) StubReply {
		if r.Outputs == 0 {
			return StubReply{Call: &StubCall{Name: "exec_command", Arguments: `{"cmd":"echo crw-real-host","sandbox_permissions":"require_escalated","justification":"crw real-host check"}`}}
		}
		return StubReply{Text: "crw real-host turn done"}
	}
}

// spawnTask is the task the script gives the agent it spawns; the agent's own model requests carry it.
const spawnTask = "crw-real-host-child-task"

// spawnScript has the turn spawn an agent, wait for it, and answer. The spawned agent's turn goes to the
// same provider; it is told apart by the task in its input.
func spawnScript(*hostScript) func(StubRequest) StubReply {
	return func(r StubRequest) StubReply {
		child := false
		for _, t := range r.Texts {
			child = child || strings.Contains(t, spawnTask)
		}
		parent := false
		for _, t := range r.Texts {
			parent = parent || strings.Contains(t, hostTurnPrompt)
		}
		if child && !parent {
			return StubReply{Text: "crw real-host child done"}
		}
		switch r.Outputs {
		case 0:
			args, _ := json.Marshal(map[string]any{"message": spawnTask, "agent_type": "worker"})
			return StubReply{Call: &StubCall{Namespace: "multi_agent_v1", Name: "spawn_agent", Arguments: string(args)}}
		case 1:
			id := agentID(r.ToolOutputs)
			args, _ := json.Marshal(map[string]any{"targets": []string{id}, "timeout_ms": 20000})
			return StubReply{Call: &StubCall{Namespace: "multi_agent_v1", Name: "wait_agent", Arguments: string(args)}}
		}
		return StubReply{Text: "crw real-host turn done"}
	}
}

// agentID is the id spawn_agent answered with, from the output of the first tool call.
func agentID(outputs []string) string {
	if len(outputs) == 0 {
		return ""
	}
	var out struct {
		AgentID string `json:"agent_id"`
		ID      string `json:"id"`
	}
	if json.Unmarshal([]byte(outputs[0]), &out) != nil {
		return ""
	}
	if out.AgentID != "" {
		return out.AgentID
	}
	return out.ID
}

var (
	turnEvents = []hostEvent{{"SessionStart", ""}, {"UserPromptSubmit", ""}, {"PreToolUse", "Bash"}, {"PostToolUse", "Bash"},
		{"PreToolUse", "view_image"}, {"PostToolUse", "view_image"}, {"Stop", ""}}
	// a compaction ends a session of the host's and starts the next one (SessionStart with source compact)
	compactionEvents = []hostEvent{{"SessionStart", ""}, {"UserPromptSubmit", ""}, {"PreToolUse", "Bash"}, {"PostToolUse", "Bash"}, {"PostCompact", ""},
		{"SessionStart", ""}, {"Stop", ""}}
	// the spawned agent has a prompt of its own, and its SubagentStop is matched against its role
	spawnEvents = []hostEvent{{"SessionStart", ""}, {"UserPromptSubmit", ""}, {"PreToolUse", "spawn_agent"}, {"UserPromptSubmit", ""}, {"PreToolUse", "wait_agent"},
		{"SubagentStop", "worker"}, {"Stop", ""}}
	permissionEvents = []hostEvent{{"SessionStart", ""}, {"UserPromptSubmit", ""}, {"PreToolUse", "Bash"}, {"Stop", ""}}
)

func hostCellSpecs() []hostCellSpec {
	return []hostCellSpec{
		{name: CellTurnCRW, state: SwitchOn, trusted: true, script: turnScript, events: turnEvents, check: checkOn},
		{name: CellTurnOff, state: SwitchOff, trusted: true, script: turnScript, events: turnEvents, check: checkSilent},
		{name: CellTurnCXC, state: SwitchCXC, trusted: true, script: turnScript, events: turnEvents, check: checkSilent},
		{name: CellUntrusted, state: SwitchOn, trusted: false, script: turnScript, check: checkUntrusted},
		{name: CellCompaction, state: SwitchOn, trusted: true, script: compactionScript, events: compactionEvents, check: checkCompaction,
			config: fmt.Sprintf("model_auto_compact_token_limit = %d\nmodel_context_window = 1000\n", hostCompactLimit)},
		{name: CellSpawn, state: SwitchOn, trusted: true, script: spawnScript, events: spawnEvents, manyTurns: true, check: checkSpawn},
		{name: CellPermission, state: SwitchOn, trusted: true, script: permissionScript, events: permissionEvents, check: checkPermission},
	}
}

// checkOn: at crw the ported legs ran and answered, the answers reached the model, the host's
// invocation records name its thread, and the turn finished.
func checkOn(cell *HostCell, r *hostRun) {
	add := func(format string, args ...any) { cell.Problems = append(cell.Problems, fmt.Sprintf(format, args...)) }
	if cell.Answered == 0 {
		add("no hook answered: the ported legs did nothing at crw")
	}
	for _, f := range cell.Firings {
		if f.Context && !f.Reached {
			add("the answer of %s (%s) is in no model request", f.Leg, f.Event)
		}
	}
	if cell.Records == 0 {
		add("the host's home holds no hook invocation record")
	}
	judgeTurn(cell, r)
}

// checkSilent: with the switch off or at cxc the host started every declared hook, and none of the
// ported legs answered, failed or recorded anything; no hook text reached the model.
func checkSilent(cell *HostCell, r *hostRun) {
	add := func(format string, args ...any) { cell.Problems = append(cell.Problems, fmt.Sprintf(format, args...)) }
	for _, f := range cell.Firings {
		if f.Leg == CompletionLeg {
			continue
		}
		if f.Answered {
			add("%s (%s) answered with the switch at %s", f.Leg, f.Event, cell.Switch)
		}
	}
	if cell.Records != 0 {
		add("%d hook invocation record(s) were written with the switch at %s", cell.Records, cell.Switch)
	}
	for _, req := range r.requests {
		for _, t := range req.Texts {
			if strings.Contains(t, "[crw") {
				add("a model request carries hook text with the switch at %s: %.60q", cell.Switch, t)
				return
			}
		}
	}
	judgeTurn(cell, r)
}

// checkUntrusted: a plugin whose hooks were never trusted runs a whole turn and no hook is started.
func checkUntrusted(cell *HostCell, r *hostRun) {
	if len(cell.Firings) != 0 || cell.Records != 0 {
		cell.Problems = append(cell.Problems, fmt.Sprintf("%d hook start(s) and %d record(s) with no trust recorded", len(cell.Firings), cell.Records))
	}
	judgeTurn(cell, r)
}

// judgeTurn: the stub's script ran to its end: the shell command and the viewer were called by the host and the answer came
// back, so the hooks above were judged against a real turn.
func judgeTurn(cell *HostCell, r *hostRun) {
	add := func(format string, args ...any) { cell.Problems = append(cell.Problems, fmt.Sprintf(format, args...)) }
	var ran, done, finished bool
	for _, it := range r.items {
		switch {
		case it.Type == "command_execution" && strings.Contains(it.Command, "crw-real-host") && it.Status == "completed":
			ran = true
		case it.Type == "agent_message" && strings.Contains(it.Text, "turn done"):
			done = true
		case it.Type == "turn.completed":
			finished = true
		}
	}
	if !ran || !done || !finished {
		add("the turn did not run to its end (command run %v, answer %v, turn completed %v)", ran, done, finished)
	}
}

// checkCompaction: the host compacted for real: the compaction request was served, the PostCompact
// legs started, and the context the recall leg injected afterwards reached the model.
func checkCompaction(cell *HostCell, r *hostRun) {
	add := func(format string, args ...any) { cell.Problems = append(cell.Problems, fmt.Sprintf(format, args...)) }
	compactions := 0
	for _, req := range r.requests {
		if req.Compaction {
			compactions++
		}
	}
	if compactions != 1 {
		add("%d compaction request(s), want 1", compactions)
	}
	var postCompact, recovered bool
	for _, f := range cell.Firings {
		postCompact = postCompact || f.Event == "PostCompact"
		recovered = recovered || (f.Event == "SessionStart" && f.Source == "compact" && f.Answered && f.Reached)
		if f.Context && !f.Reached {
			add("the answer of %s (%s) is in no model request", f.Leg, f.Event)
		}
	}
	if !postCompact {
		add("no PostCompact hook was started")
	}
	if !recovered {
		add("no answer to the SessionStart after the compaction reached the model: the context was not recovered")
	}
	var done bool
	for _, it := range r.items {
		done = done || (it.Type == "agent_message" && strings.Contains(it.Text, "after the compaction"))
	}
	if !done {
		add("the turn did not finish after the compaction")
	}
}

// checkPermission: Codex exec cannot ask for permission, and the cell says what it measured. It is a
// verified cell only when a PermissionRequest hook was started for the escalation.
func checkPermission(cell *HostCell, r *hostRun) {
	for _, f := range cell.Firings {
		if f.Event == "PermissionRequest" {
			return // the host asked: the leg ran for the host's own request
		}
	}
	for _, req := range r.requests {
		for _, out := range req.ToolOutputs {
			if strings.Contains(out, "approval policy is Never") {
				cell.Unverified = "codex exec refuses an escalated command before any permission request (\"approval policy is Never\" came back to the model), so the PermissionRequest hook is never started"
				return
			}
		}
	}
	cell.Problems = append(cell.Problems, "the escalated command was neither refused by the host as expected nor offered to a PermissionRequest hook")
}

// printRealHost is the text of the real-host cells.
func printRealHost(w io.Writer, rep RealHostReport) {
	if rep.Skipped != "" {
		fmt.Fprintf(w, "real host: no cell ran (%s)\n", rep.Skipped)
		return
	}
	for _, c := range rep.Cells {
		for _, p := range c.Problems {
			fmt.Fprintf(w, "FAIL real host %s: %s\n", c.Name, p)
		}
		status := "ok"
		switch {
		case c.Unverified != "":
			status = "NOT VERIFIED"
		case !c.OK:
			status = "FAIL"
		}
		fmt.Fprintf(w, "real host %s: %s: thread %s, %d hook start(s) %v, %d answered (%d reached the model), %d record(s), %d model request(s)\n",
			c.Name, status, c.Thread, len(c.Firings), c.Events, c.Answered, c.Reached, c.Records, c.ModelRequest)
	}
	fmt.Fprintf(w, "real host: %s (%s), bypass of hook trust: %v\n", rep.Codex.Version, rep.Codex.Path, rep.Bypass)
}

// realHostCover is a cell the isolated runs of the harness leave unverified until the real-host
// cells that cover it ran and passed: the prefix of its NotVerified cell, and the real-host cells
// that must all have run and passed to take it off the list.
type realHostCover struct {
	prefix string
	cells  []string
}

var realHostCovers = []realHostCover{
	{"real Codex binary fires the declared hook from a real turn", []string{CellTurnCRW}},
	{"hook trust: a declared hook does not run until trusted", []string{CellTurnCRW, CellUntrusted}},
	{"context recovery after a real compaction", []string{CellCompaction}},
}

// notVerifiedWithRealHost is the list of cells left unverified by a run that included the real-host
// cells. A cell leaves the list only when every real-host cell that covers it ran to a verdict and
// passed; a cell the filter left out, one that failed and one the host could not be driven into
// stay on it, and the real-host cells that did not run come in with why. A run that found no Codex
// keeps the list and adds why.
func notVerifiedWithRealHost(base []NotVerified, rh RealHostReport) []NotVerified {
	if rh.Skipped != "" {
		return append(base, rh.Unverified...)
	}
	passed := map[string]bool{}
	ran := map[string]bool{}
	for _, c := range rh.Cells {
		ran[c.Name] = true
		passed[c.Name] = c.OK && c.Unverified == ""
	}
	var out []NotVerified
	for _, n := range base {
		covered := false
		for _, cover := range realHostCovers {
			if !strings.HasPrefix(n.Cell, cover.prefix) {
				continue
			}
			covered = true
			for _, name := range cover.cells {
				covered = covered && passed[name]
			}
		}
		if !covered {
			out = append(out, n)
		}
	}
	out = append(out, rh.Unverified...)
	for _, name := range hostCellNames {
		if !ran[name] {
			out = append(out, NotVerified{"real host: " + name, "the cell did not run in this invocation (the --only filter left it out)", "run the real-host cells without --only"})
		}
	}
	return out
}

// checkSpawn: the spawned agent had a turn of its own with the provider (a request that carries its
// task and not the parent's prompt), and the parent's turn ran to its end after it. The SubagentStop
// hooks are judged with the other hooks of the cell.
func checkSpawn(cell *HostCell, r *hostRun) {
	childRequests := 0
	for _, req := range r.requests {
		task, parent := false, false
		for _, t := range req.Texts {
			task = task || strings.Contains(t, spawnTask)
			parent = parent || strings.Contains(t, hostTurnPrompt)
		}
		if task && !parent {
			childRequests++
		}
	}
	if childRequests != 1 {
		cell.Problems = append(cell.Problems, fmt.Sprintf("the spawned agent made %d model request(s), want 1", childRequests))
	}
	var done, finished bool
	for _, it := range r.items {
		done = done || (it.Type == "agent_message" && strings.Contains(it.Text, "turn done"))
		finished = finished || it.Type == "turn.completed"
	}
	if !done || !finished {
		cell.Problems = append(cell.Problems, fmt.Sprintf("the parent's turn did not run to its end after the agent stopped (answer %v, turn completed %v)", done, finished))
	}
}
