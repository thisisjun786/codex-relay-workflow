package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// MarkerRootMethod is the control.sock method that asks the relay daemon which marker root it judges Stops
// under (OwnerMarkerRoot, evaluated in the daemon's own environment). managed-start asks it before a new
// request is reserved, because the root is the daemon's to say, not the caller's environment's (CRW-1309).
const MarkerRootMethod = "marker-root"

// answerMarkerRoot answers the marker-root question with the root this owner resolves for itself, or the host
// record when it cannot resolve one. The request carries no parameter: nobody chooses the owner's root.
func answerMarkerRoot(conn net.Conn, request Object) error {
	if len(request) != 2 {
		return rejectControl(conn)
	}
	root, err := OwnerMarkerRoot()
	if err != nil {
		return answerHost(conn, err.Error())
	}
	_, err = io.WriteString(conn, pyjson.Dumps(Object{{Key: "protocol", Value: int64(1)}, {Key: "markerRoot", Value: root}}, pyjson.Options{Compact: true})+"\n")
	return err
}

// ErrOwnerMarkerRootUnanswered is wrapped by the error AskOwnerMarkerRoot returns when the daemon could not be
// asked or did not answer the question (no daemon, a daemon that rejects the method, an unreadable answer).
var ErrOwnerMarkerRootUnanswered = errors.New("the relay daemon did not answer the marker-root question")

// AskOwnerMarkerRoot asks the relay daemon serving state/control.sock for the marker root it judges Stops
// under. Any failure to get an absolute root from it wraps ErrOwnerMarkerRootUnanswered, so a caller can tell a
// daemon that said nothing from one that named a root.
func AskOwnerMarkerRoot(ctx context.Context, state string) (string, error) {
	unanswered := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("%w: %s", ErrOwnerMarkerRootUnanswered, fmt.Sprintf(format, args...))
	}
	socket := filepath.Join(state, "control.sock")
	ctx, cancel := context.WithTimeout(ctx, ControlReadTimeout)
	defer cancel()
	address, release, err := ControlAddress(socket)
	if err != nil {
		return unanswered("%s: %v", socket, err)
	}
	defer release()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			return unanswered("%s: %v; this sandbox refuses the connection to the relay daemon", socket, err)
		}
		return unanswered("%s: %v", socket, err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return unanswered("%s: %v", socket, err)
	}
	if err := authenticatePeer(ctx, conn, socket); err != nil {
		return unanswered("%s: %v", socket, err)
	}
	request := Object{{Key: "protocol", Value: int64(1)}, {Key: "method", Value: MarkerRootMethod}}
	if _, err := io.WriteString(conn, pyjson.Dumps(request, pyjson.Options{Compact: true})+"\n"); err != nil {
		return unanswered("%s: %v", socket, err)
	}
	response, err := readFrame(conn)
	if err != nil {
		return unanswered("%s: %v", socket, err)
	}
	if response.Get("requestRejected") == true {
		return unanswered("%s rejects the %s method (an older relay daemon)", socket, MarkerRootMethod)
	}
	if detail, host := response.Get("detail").(string); host && response.Get("error") != nil {
		return unanswered("%s: %s", socket, detail)
	}
	root, ok := response.Get("markerRoot").(string)
	if !ok || response.Get("protocol") != int64(1) || !filepath.IsAbs(root) {
		return unanswered("%s answered no absolute marker root", socket)
	}
	return root, nil
}
