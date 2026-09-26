package delivery

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// selectionRefusal is cli._selection_refusal: what is wrong with the store this run resolved,
// as the JSON refusal every non-exempt command prints before its handler (exit 2), or nil.

const stateEnv = "CODEX_SESSION_RELAY_STATE"

func program() string {
	name := filepath.Base(os.Args[0])
	if filepath.Dir(os.Args[0]) != "." {
		return shellQuote(os.Args[0])
	}
	return shellQuote(name)
}

func withoutStateEnv() string {
	if _, ok := os.LookupEnv(stateEnv); ok {
		return "env -u " + stateEnv + " "
	}
	return ""
}

func selectionRefusal(selection store.StateSelection, socket string) Obj {
	if socket != "" {
		if _, err := os.Stat(selection.DBPath()); err == nil {
			recorded := store.StoreSocket(selection.DBPath())
			wanted, err := store.CanonicalSocket(socket)
			if err == nil && recorded != "" && recorded != wanted {
				return Obj{{Key: "error", Value: "refused"}, {Key: "reason", Value: "state_directory_serves_another_socket"},
					{Key: "detail", Value: "this store records a different App Server socket; serving the requested one from it would expose one installation's assignments through another"},
					{Key: "recordedSocket", Value: recorded}, {Key: "requestedSocket", Value: wanted}, {Key: "stateDirectory", Value: selection.Path},
					{Key: "recover", Value: wrongSocketRecovery(selection, recorded, wanted)},
					{Key: "note", Value: "using a store does not rewrite the socket it recorded, so neither command here adopts anything; choose the matching pair"}}
			}
		}
	}
	if len(selection.Ambiguous) == 0 && len(selection.Unidentified) == 0 {
		return nil
	}
	contested := len(selection.Ambiguous) > 0
	candidates := selection.Unidentified
	reason, detail := "unidentified_state_directory", "a store here records no socket, so it cannot be ruled out as this one's; creating a new store beside it would hide it permanently"
	if contested {
		candidates = selection.Ambiguous
		reason, detail = "ambiguous_state_directory", "more than one store already records this socket, and creating a new one here would hide them both"
	}
	var socketPath any
	if socket != "" {
		socketPath = socket
	}
	list := make([]any, len(candidates))
	for i, c := range candidates {
		list[i] = c
	}
	return Obj{{Key: "error", Value: "refused"}, {Key: "reason", Value: reason}, {Key: "detail", Value: detail}, {Key: "socketPath", Value: socketPath},
		{Key: "candidates", Value: list}, {Key: "wouldHaveCreated", Value: selection.DBPath()}, {Key: "recover", Value: recoveryLines(selection, socket, candidates, contested)}}
}

func recoveryLines(selection store.StateSelection, socket string, candidates []string, contested bool) []any {
	prog := program()
	socketFlag := ""
	if socket != "" {
		socketFlag = " --socket=" + shellQuote(socket)
	}
	lines := []any{prog + socketFlag + " doctor", "  lists the candidates under siblingStores"}
	for _, c := range candidates {
		lines = append(lines, prog+" --state="+shellQuote(c)+socketFlag+" doctor", prog+" --state="+shellQuote(c)+socketFlag+" service status")
	}
	lines = append(lines, "  service status groups by project, so the candidate holding the assignments you expect is the one to keep")
	if contested {
		return append(lines, "  then pass --state=<the chosen directory> on EVERY participant of this assignment: both stores still record this socket, so default discovery keeps refusing until one of them is retired")
	}
	return append(lines, "  then pass --state="+shellQuote(selection.Path)+" once to create the new store deliberately, or --state=<the existing directory> to keep using it")
}

func wrongSocketRecovery(selection store.StateSelection, recorded, wanted string) []any {
	lines := []any{
		program() + " --state=" + shellQuote(selection.Path) + " --socket=" + shellQuote(recorded) + " doctor",
		"  reads this store under the socket it actually records",
		withoutStateEnv() + program() + " --socket=" + shellQuote(wanted) + " doctor",
		"  discovers by socket alone, ignoring any pinned directory",
	}
	pinned := os.Getenv(stateEnv)
	if pinned == "" {
		return lines
	}
	expanded := pinned
	if strings.HasPrefix(pinned, "~") {
		home, _ := os.UserHomeDir()
		if pinned != "~" && !strings.HasPrefix(pinned, "~/") {
			return append(lines, "  "+stateEnv+" is set to "+store.PyRepr(pinned)+", which names a home directory that does not resolve on this host, so it is not offered as a candidate")
		}
		expanded = home + pinned[1:]
	}
	resolved, err := filepath.Abs(expanded)
	if err == nil {
		if real, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = real
		}
	}
	current, _ := filepath.Abs(selection.Path)
	if real, err := filepath.EvalSymlinks(current); err == nil {
		current = real
	}
	if resolved != current {
		lines = append(lines, program()+" --state="+shellQuote(resolved)+" --socket="+shellQuote(wanted)+" doctor", "  reads the directory "+stateEnv+" names, which --state overrode on this run")
	}
	return lines
}
