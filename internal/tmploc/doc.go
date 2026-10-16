// Package tmploc is the one place that decides where crw puts temporary files and unix sockets
// (CRW-1566).
//
// Temporary files go below TMPDIR. Lookup is the one read of the variable; DirFrom and Dir add the
// one check a chooser needs (the value must be absolute). An unset or empty TMPDIR is DefaultDir
// ("/tmp"), reported with source "default" (G-D1 = A, Q63ANS1722). The default is a location only:
// it is never a ground for a safety judgement, and nothing here or in a caller switches to another
// place because the first one is long, unusable or below a repository. A value that is not
// absolute, or a directory that cannot be used, stops with its reason.
//
// Unix sockets never live below TMPDIR. SocketDir makes a fresh directory below SocketRoot, a short
// root fixed for every run, so a socket path's length does not depend on TMPDIR. SocketBound is the
// longest path every supported platform binds; SocketTailMax is the longest path any test puts
// below a socket directory; SocketFits is the one budget calculation.
//
// The package imports only the standard library, so every product and test-support package can
// use it without a cycle. Test helpers (testing.TB) live in tmploctest.
package tmploc
