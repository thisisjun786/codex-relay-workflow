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
	// usage writes the help page to w. The flag package calls Usage for -h/--help and for a
	// parse error alike; help goes to stdout and exits 0, an error goes to stderr and exits 2,
	// as every other crw mode does.
	usage := func(w io.Writer) {
		fmt.Fprintln(w, "usage: crw gui [-h] [--port PORT]")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "serve the local dashboard on loopback")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "options:")
		flags.SetOutput(w)
		flags.PrintDefaults()
		flags.SetOutput(stderr)
	}
	// The flag package would print the help itself, to stderr; this mode prints it to stdout on
	// -h/--help (exit 0) and to stderr on a parse error (exit 2), so the package's own usage
	// hook is silenced and this file decides the stream.
	flags.Usage = func() {}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return 0
		}
		// The flag package already wrote the error line to stderr; the usage follows it there.
		usage(stderr)
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
	// Every request context is derived from the run context, so a write handler observes the
	// cancellation that ends the server and can check it right before a durable effect; a
	// handler waiting on cancellation also lets the graceful shutdown finish instead of
	// outliving it.
	httpServer := &http.Server{
		Handler:           server.Handler(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		// net/http answers `OPTIONS *` itself with a general OPTIONS handler that never
		// consults Handler, so without this the request would skip the guard, the Host check
		// and every security header. Disabling it sends the request through the guard.
		DisableGeneralOptionsHandler: true,
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), server.shutdown)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			// A handler that outlasted the grace period is ended rather than left running: the
			// listener is already closed, so nothing new starts, and Close ends what remains.
			_ = httpServer.Close()
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
