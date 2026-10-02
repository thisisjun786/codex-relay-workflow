package dagsched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay commands of the DAG scheduler (docs/relay/dag-scheduler.md). Every answer is the relay's JSON envelope; a refusal is exit 2 with an existing
// reason, a document that is not JSON is a usage error (exit 4) and a stored plan that disagrees with itself is the host's failure (exit 3).

func init() {
	dispatch.Register(nil,
		// dag-ready reads, and writes only when it is asked to keep the pass.
		dispatch.Command{Name: "dag-ready", ReadOnlyWhen: func(args dispatch.Args) bool { return !args.Bool("record") }, Run: runReady},
		dispatch.Command{Name: "dag-region-declare", Run: runRegionDeclare},
		// dag-release starts a managed task: like managed-start it opens its own admitted connection and needs the explicit --state and --socket.
		dispatch.Command{Name: "dag-release", OwnAdmission: true, Exempt: true, Run: runRelease},
	)
}

func usage(detail string) error {
	return &dispatch.UsageError{Detail: detail, Code: contract.ExitUsage}
}

// hostFailure answers a plan that does not agree with itself as the host's failure.
func hostFailure(err error) error {
	var corrupt *dag.CorruptError
	if errors.As(err, &corrupt) {
		return dispatch.Host(corrupt.Error())
	}
	return err
}

// openScheduler opens the selected store for a scheduler command. Selectors are the spellings a release fingerprints; commands that do not release leave them empty.
func openScheduler(ctx context.Context, services dispatch.Services) (*Scheduler, func(), error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, nil, err
	}
	return &Scheduler{Store: s}, func() { _ = s.Close() }, nil
}

func runReady(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	record := args.Bool("record")
	actor := args.Text("actor")
	if record && actor == "" {
		return nil, usage("--record needs --actor: a recorded pass names who asked")
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	if !record {
		reading, err := sched.Read(ctx, args.Text("plan"), ReadyOptions{})
		if err != nil {
			return nil, hostFailure(err)
		}
		return reading.Object(), nil
	}
	reading, seq, err := sched.RecordPass(ctx, args.Text("plan"), actor, ReadyOptions{})
	if err != nil {
		return nil, hostFailure(err)
	}
	return append(reading.Object(), contract.Field{Key: "pass_seq", Value: seq}), nil
}

// readDocument is a JSON option's value: the text itself, or @file.
func readDocument(input string) ([]byte, error) {
	if !strings.HasPrefix(input, "@") {
		if err := store.EncodeUTF8(input); err != nil {
			return nil, usage(err.Error())
		}
		return []byte(input), nil
	}
	file, err := os.Open(input[1:])
	if err != nil {
		return nil, usage(err.Error())
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, dag.MaxDocumentBytes+1))
	if err != nil {
		return nil, usage(err.Error())
	}
	return raw, nil
}

// wireRegion is one region of the --regions document.
type wireRegion struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Key        string `json:"key"`
	Change     string `json:"change"`
	Exclusive  bool   `json:"exclusive"`
}

func decodeRegions(raw []byte) ([]Region, error) {
	var wire []wireRegion
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, usage("--regions is a JSON list of {repository, path, kind, key, change, exclusive}: " + err.Error())
	}
	if decoder.More() {
		return nil, usage("--regions holds one JSON list")
	}
	out := make([]Region, len(wire))
	for i, w := range wire {
		out[i] = Region(w)
	}
	return out, nil
}

func runRegionDeclare(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	raw, err := readDocument(args.Text("regions"))
	if err != nil {
		return nil, err
	}
	regions, err := decodeRegions(raw)
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	declared, err := sched.DeclareRegions(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), regions)
	if err != nil {
		return nil, hostFailure(err)
	}
	list := make([]any, len(declared.Regions))
	for i, r := range declared.Regions {
		list[i] = contract.OrderedObject{{Key: "repository", Value: r.Repository}, {Key: "path", Value: r.Path}, {Key: "kind", Value: r.Kind},
			{Key: "key", Value: optionalText(r.Key)}, {Key: "change", Value: r.Change}, {Key: "exclusive", Value: r.Exclusive}}
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "plan_id", Value: declared.PlanID}, {Key: "node_id", Value: declared.NodeID},
		{Key: "declaration_seq", Value: declared.Seq}, {Key: "replayed", Value: declared.Replayed}, {Key: "regions", Value: list}}, nil
}

func runRelease(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	if services.SocketPath == "" || services.Selection.Source != "flag" {
		return nil, usage("dag-release starts a managed task and requires explicit --state and --socket")
	}
	raw, err := readDocument(args.Text("request"))
	if err != nil {
		return nil, err
	}
	request, err := DecodeReleaseRequest(raw)
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	sched.Tips = mergeturn.TargetReader{}
	sched.PRs = ForgePullRequestReader(ExecRunner)
	sched.Start = ProductionStarter(services, args)
	sched.Selectors = Selectors{MarkerRoot: args.Text("marker-root"), Socket: services.SocketPath, StateSelector: services.Selection.Path}
	result, err := sched.Release(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), request)
	if err != nil {
		return nil, hostFailure(err)
	}
	if !result.Bound {
		// the managed start was refused or left incomplete: the intent and the slot stay, and the same call again continues it.
		return nil, &dispatch.PayloadExit{Payload: result.Object(), Code: contract.ExitRefused}
	}
	return result.Object(), nil
}
