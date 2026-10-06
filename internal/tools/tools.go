package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// The exit statuses this command answers with. install and path share the pinned identities; the
// usage errors are 2, as they are for every crw mode.
const (
	usageExit = 2
	// notInstalledExit is path's answer for a tool that is not installed, and the status of a
	// host failure this command cannot classify.
	notInstalledExit = 1
	// digestMismatchExit is install's answer for an archive whose sha256 is not the pin's.
	digestMismatchExit = 3
)

// recordFile is the installed record crw writes beside the executable.
const recordFile = "pin.json"

// fetchTimeout bounds one download.
const fetchTimeout = 120 * time.Second

// maxArchiveBytes bounds what is read into memory: the archive is verified and unpacked from the
// same bytes, so nothing can change between the digest and the unpack.
const maxArchiveBytes = 256 << 20

// stagingPrefix names the directory an install stages into under tools_root. It stays inside the
// tools root so the rename into the final directory never crosses a filesystem.
const stagingPrefix = ".staging-"

// executableMode is the mode an installed executable gets.
const executableMode os.FileMode = 0o755

// recordMode is the mode the installed record gets.
const recordMode os.FileMode = 0o644

// Seams are the host the command runs against. A nil field takes its production value, so the
// command needs no seam set to run, and a test replaces only what it observes.
type Seams struct {
	// URLBase is where an archive is fetched from; empty means ReleaseBase.
	URLBase string
	// Client is the HTTP client an archive is fetched with; nil means http.DefaultClient.
	Client *http.Client
	// GOOS and GOARCH are the platform the pin is matched against; empty means the running one.
	GOOS, GOARCH string
	// Now is the clock an install records; nil means time.Now.
	Now func() time.Time
	// Getenv reads the environment the configuration is resolved from; nil means os.Getenv.
	Getenv func(string) string
}

func (s *Seams) platform() (string, string) {
	goos, goarch := s.GOOS, s.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func (s *Seams) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s *Seams) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Seams) getenv(name string) string {
	if s.Getenv != nil {
		return s.Getenv(name)
	}
	return os.Getenv(name)
}

func (s *Seams) urlBase() string {
	if s.URLBase != "" {
		return strings.TrimSuffix(s.URLBase, "/")
	}
	return ReleaseBase
}

// failure is a refused command: the reason the product names, a detail a person reads, and the
// exit status. The reason is a product token (not_installed, digest_mismatch,
// unsupported_platform); a failure with no reason is a host failure and carries only its detail.
type failure struct {
	reason string
	detail string
	status int
}

func (f *failure) Error() string {
	if f.reason == "" {
		return f.detail
	}
	return f.reason + ": " + f.detail
}

// refuse builds a failure carrying a product reason.
func refuse(reason string, status int, format string, args ...any) *failure {
	return &failure{reason: reason, status: status, detail: fmt.Sprintf(format, args...)}
}

// hostFail builds a failure with no product reason: a host or input failure this command cannot
// classify, which exits 1.
func hostFail(format string, args ...any) *failure {
	return &failure{status: notInstalledExit, detail: fmt.Sprintf(format, args...)}
}

// statusOf reads the exit status an error carries; anything this package did not raise is a host
// failure.
func statusOf(err error) int {
	var f *failure
	if errors.As(err, &f) {
		return f.status
	}
	return notInstalledExit
}

// roots resolves the configured roots the command installs under. A configuration this command
// cannot use is a usage-class error, as it is for crw config.
func roots(seams *Seams) (map[string]crwconfig.Root, error) {
	file, err := crwconfig.Load(seams.getenv, "")
	if err != nil {
		return nil, err
	}
	return file.Roots(), nil
}

// listRow is one pin's row in the crw tools list report.
type listRow struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Archive    string `json:"archive"`
	SHA256     string `json:"sha256"`
	Executable string `json:"executable"`
	Installed  bool   `json:"installed"`
	Path       string `json:"path"`
}

// listReport is the crw tools list document.
type listReport struct {
	Pins []listRow `json:"pins"`
}

// Run is crw tools: install, path and list over the pin table in pins.go. Every subcommand writes
// its answer to stdout and exits 0 on success; a refusal carries its product reason and its own
// exit status (2 usage or unsupported_platform, 1 not_installed or a host failure, 3
// digest_mismatch).
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, seams *Seams) int {
	if seams == nil {
		seams = &Seams{}
	}
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw tools: error: the following arguments are required: command")
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	}
	verb := args[0]
	switch verb {
	case "install", "path", "list":
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw tools: error: invalid command %q (choose from 'install', 'path', 'list')\n", verb)
		return usageExit
	}
	rest := args[1:]
	if verb == "list" {
		if len(rest) > 0 {
			return unexpected(stderr, rest[0])
		}
		return runList(seams, stdout, stderr)
	}
	if len(rest) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw tools: error: the following arguments are required: name")
		return usageExit
	}
	if len(rest) > 1 {
		return unexpected(stderr, rest[1])
	}
	name := rest[0]
	pin, ok := Lookup(name)
	if !ok {
		usage(stderr)
		fmt.Fprintf(stderr, "crw tools: error: invalid tool %q (choose from %s)\n", name, choiceList())
		return usageExit
	}
	resolved, err := roots(seams)
	if err != nil {
		fmt.Fprintf(stderr, "crw tools: error: %v\n", err)
		return usageExit
	}
	toolsRoot := resolved[crwconfig.RootTools].Path
	var runErr error
	switch verb {
	case "install":
		var path string
		path, runErr = install(ctx, pin, seams, toolsRoot, resolved[crwconfig.RootTemp].Path)
		if runErr == nil {
			fmt.Fprintf(stdout, "%s\n", path)
		}
	case "path":
		var path string
		path, runErr = installedPath(pin, toolsRoot)
		if runErr == nil {
			fmt.Fprintf(stdout, "%s\n", path)
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "crw tools %s: error: %v\n", verb, runErr)
		return statusOf(runErr)
	}
	return 0
}

// unexpected is the answer to an argument this command does not take.
func unexpected(stderr io.Writer, arg string) int {
	usage(stderr)
	fmt.Fprintf(stderr, "crw tools: error: unexpected argument %q\n", arg)
	return usageExit
}

// runList writes the pin table with each pin's install state.
func runList(seams *Seams, stdout, stderr io.Writer) int {
	resolved, err := roots(seams)
	if err != nil {
		fmt.Fprintf(stderr, "crw tools: error: %v\n", err)
		return usageExit
	}
	toolsRoot := resolved[crwconfig.RootTools].Path
	report := listReport{Pins: make([]listRow, 0, len(Pins))}
	for _, pin := range Pins {
		row := listRow{
			Name: pin.Name, Version: pin.Version, Archive: pin.Archive, SHA256: pin.SHA256,
			Executable: pin.Executable,
		}
		if path, err := installedPath(pin, toolsRoot); err == nil {
			row.Installed, row.Path = true, path
		}
		report.Pins = append(report.Pins, row)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "crw tools list: error: %v\n", err)
		return notInstalledExit
	}
	fmt.Fprintf(stdout, "%s\n", data)
	return 0
}

// usage writes the usage line and one line per subcommand.
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw tools [-h] {install,path,list} ...")
	fmt.Fprintln(w, "  install\tdownload, verify and install a pinned tool: crw tools install <name>")
	fmt.Fprintln(w, "  path\tprint the installed executable's path: crw tools path <name>")
	fmt.Fprintln(w, "  list\tprint every pinned tool and whether it is installed")
}
