package shellir

import "strings"

// stdlibModuleNames is every top-level module name of the standard library a Python 3.8 to 3.14 interpreter can import (the public
// names of sys.stdlib_module_names and sys.builtin_module_names of Python 3.14.4, plus the modules earlier versions had and 3.14 removed,
// plus the site-startup modules and the packages a virtual environment imports at start). It is the set the shadow check of the
// fixed module runs that read no inventory (json.tool, py_compile) compares the working directory's entries with (CRW-1187): a file,
// a compiled file, an extension or a package of the directory with one of these names is what the interpreter imports at start,
// since the directory is sys.path[0] under -m. A name that starts with an underscore is a shadow too (stdlibShadowName): the private
// modules (_colorize, _pyrepl, _weakref) are not listed, so a version that adds one is covered. The list is pinned to the
// interpreter on the host by a test that runs python3 in a temporary home; a name a newer interpreter adds fails it until it is added.
var stdlibModuleNames = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`
	abc aifc annotationlib antigravity argparse array ast asynchat asyncio asyncore atexit audioop base64 bdb binascii
	binhex bisect builtins bz2 cProfile calendar cgi cgitb chunk cmath cmd code codecs codeop collections colorsys
	compileall compression concurrent configparser contextlib contextvars copy copyreg crypt csv ctypes curses dataclasses
	datetime dbm decimal difflib dis distutils doctest dummy_threading email encodings ensurepip enum errno faulthandler
	fcntl filecmp fileinput fnmatch formatter fractions ftplib functools gc genericpath getopt getpass gettext glob graphlib
	grp gzip hashlib heapq hmac html http idlelib imaplib imghdr imp importlib inspect io ipaddress itertools json keyword
	lazy lib2to3 linecache locale logging lzma macpath mailbox mailcap marshal math mimetypes mmap modulefinder msilib
	msvcrt multiprocessing netrc nis nntplib nt ntpath nturl2path numbers opcode operator optparse os ossaudiodev parser
	pathlib pdb pickle pickletools pip pipes pkg_resources pkgutil platform plistlib poplib posix posixpath pprint profile
	profiling pstats pty pwd py_compile pyclbr pydoc pydoc_data pyexpat queue quopri random re readline reprlib resource
	rlcompleter runpy sched secrets select selectors setuptools shelve shlex shutil signal site sitecustomize smtpd smtplib
	sndhdr socket socketserver spwd sqlite3 sre_compile sre_constants sre_parse ssl stat statistics string stringprep struct
	subprocess sunau symbol symtable sys sysconfig syslog tabnanny tarfile telnetlib tempfile termios test textwrap this
	threading time timeit tkinter token tokenize tomllib trace traceback tracemalloc tty turtle turtledemo types typing
	unicodedata unittest urllib usercustomize uu uuid venv warnings wave weakref webbrowser winreg winsound wsgiref xdrlib
	xml xmlrpc zipapp zipfile zipimport zlib zoneinfo
`) {
		m[n] = true
	}
	return m
}()

// stdlibShadowName is whether a file or directory base name (without its extensions) is a module name the interpreter can import
// from the standard library at start: a listed name, or any private (underscore) name.
func stdlibShadowName(base string) bool {
	switch base {
	case "__init__", "__main__", "__pycache__":
		return false // never imported by that name from sys.path[0]: a package's own file, the running module, the cache directory
	}
	return stdlibModuleNames[base] || strings.HasPrefix(base, "_")
}
