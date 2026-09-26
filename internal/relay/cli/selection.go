package cli

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// selectionRefusal is _selection_refusal: what is wrong with the store this run resolved for
// itself, as a payload, or nil.
func selectionRefusal(services Services) (contract.OrderedObject, error) {
	selection := services.Selection
	if services.SocketPath != "" && fileExists(selection.DBPath()) {
		recorded := store.StoreSocket(selection.DBPath())
		wanted, err := store.CanonicalSocket(services.SocketPath)
		if err != nil {
			return nil, err
		}
		if recorded != "" && recorded != wanted {
			return contract.OrderedObject{
				{Key: "error", Value: "refused"},
				{Key: "reason", Value: "state_directory_serves_another_socket"},
				{Key: "detail", Value: "this store records a different App Server socket; serving the requested" +
					" one from it would expose one installation's assignments through another"},
				{Key: "recordedSocket", Value: recorded},
				{Key: "requestedSocket", Value: wanted},
				{Key: "stateDirectory", Value: selection.Path},
				{Key: "recover", Value: wrongSocketRecovery(services, recorded, wanted)},
				{Key: "note", Value: "using a store does not rewrite the socket it recorded, so neither" +
					" command here adopts anything; choose the matching pair"},
			}, nil
		}
	}
	if len(selection.Ambiguous) == 0 && len(selection.Unidentified) == 0 {
		return nil, nil
	}
	contested := len(selection.Ambiguous) > 0
	reason, detail, candidates := "unidentified_state_directory",
		"a store here records no socket, so it cannot be ruled out as this one's;"+
			" creating a new store beside it would hide it permanently", selection.Unidentified
	if contested {
		reason, detail, candidates = "ambiguous_state_directory",
			"more than one store already records this socket, and creating a new one here"+
				" would hide them both", selection.Ambiguous
	}
	return contract.OrderedObject{
		{Key: "error", Value: "refused"},
		{Key: "reason", Value: reason},
		{Key: "detail", Value: detail},
		{Key: "socketPath", Value: nullableText(services.SocketPath)},
		{Key: "candidates", Value: stringList(candidates)},
		{Key: "wouldHaveCreated", Value: selection.DBPath()},
		{Key: "recover", Value: recoveryCommands(services, contested)},
	}, nil
}

// recoveryCommands is _recovery_commands for every caller but guard-evaluate.
func recoveryCommands(services Services, contested bool) []any {
	socket := ""
	if services.SocketPath != "" {
		socket = " --socket=" + shellQuote(services.SocketPath)
	}
	lines := []any{services.Program + socket + " doctor", "  lists the candidates under siblingStores"}
	candidates := services.Selection.Unidentified
	if contested {
		candidates = services.Selection.Ambiguous
	}
	for _, candidate := range candidates {
		quoted := shellQuote(candidate)
		lines = append(lines, services.Program+" --state="+quoted+socket+" doctor",
			services.Program+" --state="+quoted+socket+" service status")
	}
	lines = append(lines, "  service status groups by project, so the candidate holding the assignments you"+
		" expect is the one to keep")
	if contested {
		return append(lines, "  then pass --state=<the chosen directory> on EVERY participant of this"+
			" assignment: both stores still record this socket, so default discovery keeps"+
			" refusing until one of them is retired")
	}
	return append(lines, "  then pass --state="+shellQuote(services.Selection.Path)+" once to create the new"+
		" store deliberately, or --state=<the existing directory> to keep using it")
}

// wrongSocketRecovery is _wrong_socket_recovery.
func wrongSocketRecovery(services Services, recorded, wanted string) []any {
	without := ""
	if _, set := os.LookupEnv(store.StateEnv); set {
		without = "env -u " + store.StateEnv + " "
	}
	lines := []any{
		services.Program + " --state=" + shellQuote(services.Selection.Path) + " --socket=" + shellQuote(recorded) + " doctor",
		"  reads this store under the socket it actually records",
		without + services.Program + " --socket=" + shellQuote(wanted) + " doctor",
		"  discovers by socket alone, ignoring any pinned directory",
	}
	pinned := os.Getenv(store.StateEnv)
	if pinned == "" {
		return lines
	}
	expanded, err := store.ExpandUser(pinned)
	var resolved string
	if err == nil {
		resolved, err = store.ResolvePath(expanded)
	}
	if err != nil {
		return append(lines, "  "+store.StateEnv+" is set to "+store.PythonRepr(pinned)+", which names a home directory that does not"+
			" resolve on this host, so it is not offered as a candidate")
	}
	selected, err := store.ResolvePath(services.Selection.Path)
	if err != nil || resolved != selected {
		lines = append(lines,
			services.Program+" --state="+shellQuote(resolved)+" --socket="+shellQuote(wanted)+" doctor",
			"  reads the directory "+store.StateEnv+" names, which --state overrode on this run")
	}
	return lines
}
