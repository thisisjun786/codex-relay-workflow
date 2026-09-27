package cli

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Reporting observation belongs to todo 24. omitted.py was carried from todo 21.
var reportingShowCommand = Command{
	Name: "reporting-show", Exempt: true,
	Required: []string{"marker-root", "workspace", "assignment", "session", "turn"},
	Flags: func(f *flag.FlagSet) {
		for _, name := range []string{"marker-root", "workspace", "assignment", "session", "turn"} {
			f.String(name, "", "")
		}
	},
	Run: func(ctx context.Context, services Services, args Args) (any, error) {
		if services.Selection.Source != "flag" {
			return nil, &UsageError{Detail: "reporting-show requires explicit --state", Code: contract.ExitUsage}
		}
		if services.SocketPath != "" {
			return nil, &UsageError{Detail: "reporting-show does not take --socket", Code: contract.ExitUsage}
		}
		assignment, _ := args.String("assignment")
		session, _ := args.String("session")
		turn, _ := args.String("turn")
		root, _ := args.String("marker-root")
		workspace, _ := args.String("workspace")
		if len(assignment) != 64 || !hexDispatch(assignment) {
			return nil, &UsageError{Detail: "assignment must be a dispatch hash", Code: contract.ExitUsage}
		}
		if !validSegment(session) || !validSegment(turn) {
			return nil, &UsageError{Detail: "session and turn must be valid path segments", Code: contract.ExitUsage}
		}
		if root == "" || workspace == "" {
			return nil, &UsageError{Detail: "marker root and workspace are required", Code: contract.ExitUsage}
		}
		return delivery.ObserveOmission(ctx, services.Selection, root, workspace, assignment, session, turn, delivery.ISOOf(clockNow()), 0), nil
	},
}

var reportingDeriveCommand = Command{
	Name: "reporting-derive", Exempt: true, Required: []string{"relationship"},
	Flags: func(f *flag.FlagSet) {
		f.String("relationship", "", "")
		f.String("turn", "", "")
		f.Float64("grace", 300, "")
	},
	Run: func(ctx context.Context, services Services, args Args) (any, error) {
		if services.SocketPath != "" {
			return nil, &UsageError{Detail: "reporting-derive does not take --socket", Code: contract.ExitUsage}
		}
		path := services.Selection.DBPath()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			rid, _ := args.String("relationship")
			return contract.OrderedObject{{Key: "schema", Value: delivery.OmittedSchema}, {Key: "source", Value: delivery.OmittedStoreSource}, {Key: "reportingState", Value: "unmeasured"}, {Key: "reason", Value: "store_absent"}, {Key: "relationshipId", Value: rid}, {Key: "observedAt", Value: delivery.ISOOf(clockNow())}, {Key: "owed", Value: false}, {Key: "owedReason", Value: delivery.OmittedNotOwed}, {Key: "detail", Value: "no relay store exists at " + path + "; nothing was created"}}, nil
		} else if err != nil {
			return nil, err
		}
		s, err := store.Open(ctx, path, "")
		if err != nil {
			return nil, err
		}
		defer s.Close()
		rid, _ := args.String("relationship")
		turn, _ := args.String("turn")
		grace := args.Flags.Lookup("grace").Value.(flag.Getter).Get().(float64)
		return delivery.DeriveOmission(ctx, s, services.Selection.Path, rid, turn, delivery.ISOOf(clockNow()), grace), nil
	},
}

func hexDispatch(s string) bool {
	for _, ch := range s {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return false
		}
	}
	return true
}
func validSegment(s string) bool {
	return strings.TrimSpace(s) != "" && s != "." && s != ".." && filepath.Base(s) == s && !strings.ContainsAny(s, "\\/\x00")
}
