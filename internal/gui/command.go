package gui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// usageExit is the status of a command line this command cannot use, as every other crw mode
// reports one.
const usageExit = 2

// Run is `crw gui [--port N]`: it binds loopback on an empty port by default, prints the one
// token-bearing URL line, and serves until the context is cancelled. It returns the process
// exit status: 0 on a clean shutdown, 1 when the server cannot start, 2 on a usage error.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw gui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	port := flags.Int("port", 0, "the loopback port to bind (0 chooses a free one)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: crw gui [-h] [--port PORT]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "serve the local dashboard on loopback")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "options:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageExit
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw gui: unrecognized arguments: %s\n", joinArgs(flags.Args()))
		return usageExit
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "crw gui: error: --port must be between 0 and 65535, not %d\n", *port)
		return usageExit
	}
	if err := Serve(ctx, Options{Port: *port}, stdout); err != nil {
		fmt.Fprintln(stderr, "crw gui: error:", err)
		return 1
	}
	return 0
}

// Serve binds the loopback listener, prints the one URL line, and serves until ctx is
// cancelled, then closes the listener and waits briefly for in-flight requests. The URL and
// the token are printed and never written to a file, and no browser is opened.
func Serve(ctx context.Context, opts Options, stdout io.Writer) error {
	server, err := New(opts)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(opts.Port))
	if err != nil {
		return fmt.Errorf("gui: cannot bind 127.0.0.1:%d: %w", opts.Port, err)
	}
	defer listener.Close()
	port, err := loopbackPort(listener.Addr())
	if err != nil {
		return err
	}
	server.port = port
	fmt.Fprintln(stdout, "crw gui: serving "+server.URL())
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), server.shutdown)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			return fmt.Errorf("gui: the shutdown did not finish: %w", err)
		}
		return nil
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// URL is the one line this server prints: the loopback address, the bound port and the
// per-run token in the fragment, which the screen moves to sessionStorage and clears.
func (s *Server) URL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.port) + "/#token=" + s.token
}

// joinArgs renders the leftover arguments for the usage message.
func joinArgs(args []string) string {
	out := ""
	for i, arg := range args {
		if i > 0 {
			out += " "
		}
		out += arg
	}
	return out
}
