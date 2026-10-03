// Package selection shares the CLI's receipt-store selection refusals with hooks.
package selection

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Services struct {
	Selection           store.StateSelection
	SocketPath, Program string
}

// SocketMismatch is Refusal's first half: the refusal of a selected store that records another
// App Server socket than the one it must serve, or nil. That socket is --socket, or the default
// App Server socket that scoped a discovery made without one (decision 73), which is compared only:
// nothing here or after it connects to it. It applies to every command that uses the selected
// store, those exempt from the discovery refusals included; doctor and service status report it
// rather than refuse (Mismatch).
func SocketMismatch(services Services) (contract.OrderedObject, error) {
	selection := services.Selection
	socket, detail := services.SocketPath, "this store records a different App Server socket; serving the requested"+
		" one from it would expose one installation's assignments through another"
	if socket == "" && selection.DefaultSocket != "" {
		socket, detail = selection.DefaultSocket, "this store records a different App Server socket than the default one"+
			" that scoped its directory; serving it to a command given no --socket would expose one"+
			" installation's assignments through another"
	}
	if socket == "" || !fileExists(selection.DBPath()) {
		return nil, nil
	}
	recorded := store.StoreSocket(selection.DBPath())
	wanted, err := store.CanonicalSocket(socket)
	if err != nil {
		return nil, err
	}
	if recorded == "" || recorded == wanted {
		return nil, nil
	}
	return contract.OrderedObject{
		{Key: "error", Value: "refused"},
		{Key: "reason", Value: "state_directory_serves_another_socket"},
		{Key: "detail", Value: detail},
		{Key: "recordedSocket", Value: recorded},
		{Key: "requestedSocket", Value: wanted},
		{Key: "stateDirectory", Value: selection.Path},
		{Key: "recover", Value: wrongSocketRecovery(services, recorded, wanted)},
		{Key: "note", Value: "using a store does not rewrite the socket it recorded, so neither" +
			" command here adopts anything; choose the matching pair"},
	}, nil
}

// Mismatch is SocketMismatch as a diagnostic reports it (doctor's and service status's
// socketMismatch): the same fields without "error", or nil, also when the socket cannot be
// resolved, which leaves nothing to compare.
func Mismatch(services Services) contract.OrderedObject {
	refusal, err := SocketMismatch(services)
	if refusal == nil || err != nil {
		return nil
	}
	return refusal[1:]
}

// Refusal is _selection_refusal: what is wrong with the selected store, or nil.
func Refusal(services Services) (contract.OrderedObject, error) {
	selection := services.Selection
	if refusal, err := SocketMismatch(services); refusal != nil || err != nil {
		return refusal, err
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

// recoveryCommands is _recovery_commands for every caller but the Stop guard's fallback
// (GuardFallback), whose refusal says the Stop was released unjudged (stopRecovery).
func recoveryCommands(services Services, contested bool) []any {
	socket := ""
	if services.SocketPath != "" {
		socket = " --socket=" + quote.Shell(services.SocketPath)
	}
	lines := []any{services.Program + socket + " doctor", "  lists the candidates under siblingStores"}
	candidates := services.Selection.Unidentified
	if contested {
		candidates = services.Selection.Ambiguous
	}
	for _, candidate := range candidates {
		quoted := quote.Shell(candidate)
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
	return append(lines, "  then pass --state="+quote.Shell(services.Selection.Path)+" once to create the new"+
		" store deliberately, or --state=<the existing directory> to keep using it")
}

// wrongSocketRecovery is _wrong_socket_recovery.
func wrongSocketRecovery(services Services, recorded, wanted string) []any {
	without := ""
	if _, set := os.LookupEnv(store.StateEnv); set {
		without = "env -u " + store.StateEnv + " "
	}
	lines := []any{
		services.Program + " --state=" + quote.Shell(services.Selection.Path) + " --socket=" + quote.Shell(recorded) + " doctor",
		"  reads this store under the socket it actually records",
		without + services.Program + " --socket=" + quote.Shell(wanted) + " doctor",
		"  discovers by socket alone, ignoring any pinned directory",
	}
	pinned := os.Getenv(store.StateEnv)
	if pinned == "" {
		return lines
	}
	// Path(pinned).expanduser().resolve(), which keeps a component it cannot examine or a link
	// loop as spelled (store.Realpath): only an unknown ~user is said rather than offered.
	expanded, err := store.ExpandUser(pinned)
	var resolved string
	if err == nil {
		resolved, err = store.Realpath(expanded)
	}
	if err != nil {
		return append(lines, "  "+store.StateEnv+" is set to "+pyvalue.StrRepr(pinned)+", which names a home directory that does not"+
			" resolve on this host, so it is not offered as a candidate")
	}
	selected, err := store.Realpath(services.Selection.Path)
	if err != nil || resolved != selected {
		lines = append(lines,
			services.Program+" --state="+quote.Shell(resolved)+" --socket="+quote.Shell(wanted)+" doctor",
			"  reads the directory "+store.StateEnv+" names, which --state overrode on this run")
	}
	return lines
}

var stopRecovery = []any{
	"  this Stop was released without being judged. It is not judged again later, and a --state or CODEX_SESSION_RELAY_STATE does not change that: a directory chosen for one run is not one anybody recorded",
	"  a later Stop of this assignment is judged against a store when the hook's settings name it with --db-path, or the intent records it as dbPath (both are written once, so neither can be added to an existing installation or assignment), or when discovery for this socket names exactly one store and nothing pins another directory for the hook",
	"  which of the candidates holds this assignment is not something this refusal can establish; the lines above read them without changing anything",
}

func GuardFallback(s Services) (string, error) {
	refusal, err := Refusal(s)
	if err != nil {
		return "", err
	}
	overridden := false
	if refusal == nil && s.SocketPath != "" && (s.Selection.Source == "flag" || s.Selection.Source == "env") {
		discovered, err := store.DiscoverStateDir(s.SocketPath)
		if err != nil {
			return "", err
		}
		if len(discovered.Ambiguous) > 0 || len(discovered.Unidentified) > 0 {
			copy := s
			copy.Selection = discovered
			refusal, err = Refusal(copy)
			if err != nil {
				return "", err
			}
			overridden = true
			out := contract.OrderedObject{}
			for _, f := range refusal {
				if f.Key != "wouldHaveCreated" {
					out = append(out, f)
				}
			}
			refusal = out
			refusal = append(refusal, contract.Field{Key: "overriddenBy", Value: s.Selection.Detail}, contract.Field{Key: "selectedDirectory", Value: s.Selection.Path})
			for i := range refusal {
				if refusal[i].Key == "detail" {
					refusal[i].Value = pyvalue.Str(refusal[i].Value) + "; this run's state directory came from " + s.Selection.Detail + ", which skips discovery rather than settling it, and a directory chosen for one run is not one anybody recorded"
				}
			}
		}
	}
	if refusal == nil {
		return s.Selection.DBPath(), nil
	}
	for i := range refusal {
		if refusal[i].Key == "recover" {
			lines, _ := evidence.List(refusal[i].Value)
			if len(lines) > 0 && strings.HasPrefix(pyvalue.Str(lines[len(lines)-1]), "  then pass --state") {
				lines = lines[:len(lines)-1]
				lines = append(lines, stopRecovery...)
				if _, present := os.LookupEnv(store.StateEnv); present {
					lines[0] = "env -u " + store.StateEnv + " " + pyvalue.Str(lines[0])
				}
			}
			if overridden {
				lines = append(lines, "  this run's directory came from "+s.Selection.Detail+". An override is read before discovery on every Stop that carries it, so while it is set, discovery naming one store does not decide which store is read")
			}
			refusal[i].Value = lines
		}
	}
	suffix := " this selection"
	if overridden {
		suffix = " a state directory chosen for this run, while discovery for this socket does not name one store"
	}
	refusal = append(refusal, contract.Field{Key: "stopNotJudged", Value: "this Stop was neither classified nor recorded: no --db-path named a receipt store and the coordinator recorded none in the intent, so the only candidate left was" + suffix})
	return "", &Refused{Payload: refusal}
}

// Program is the shell-rendered command prefix the recovery commands name: how the operator
// invoked the relay CLI (argv0), the multi-call `crw relay` spelled with the executable it ran.
func Program(argv0 string) string {
	if argv0 == "crw relay" {
		argv0 = os.Args[0]
		if !strings.Contains(argv0, "/") {
			argv0 = filepath.Base(argv0)
		}
		return quote.Shell(argv0) + " relay"
	}
	if strings.Contains(argv0, "/") {
		return quote.Shell(argv0)
	}
	return quote.Shell(filepath.Base(argv0))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Refused carries the complete Python selection envelope.
type Refused struct{ Payload contract.OrderedObject }

func (e *Refused) Error() string { return pyvalue.Str(e.Payload.Get("detail")) }
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func stringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
