package store

import (
	"os"
	"syscall"
	"testing"
)

// PathRepr is repr(os.fsdecode(name)) for the bytes a Go path holds, and an OSError's text names
// its filename that way. Each expectation is CPython 3.14.4's answer for the same bytes.
func TestPathReprIsPythonsReprOfTheDecodedFilename(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"/tmp/plain", `'/tmp/plain'`},
		{"/tmp/a\u00a0b", `'/tmp/a\xa0b'`},
		{"/tmp/a\u2028b", `'/tmp/a\u2028b'`},
		{"/tmp/a\xffb", `'/tmp/a\udcffb'`},
		{"/tmp/a\xed\xa0\x80b", `'/tmp/a\udced\udca0\udc80b'`},
		{"/tmp/it's", `"/tmp/it's"`},
		{"/tmp/it's \"q\"", `'/tmp/it\'s "q"'`},
		{"/tmp/t\tn\nr\r\\", `'/tmp/t\tn\nr\r\\'`},
		{"/tmp/\x01\x7f", `'/tmp/\x01\x7f'`},
		{"/tmp/\xe2\x82", `'/tmp/\udce2\udc82'`},
		{"/tmp/\ufffd", "'/tmp/\ufffd'"},
		{"/tmp/\U0001f600", "'/tmp/\U0001f600'"},
		{"/tmp/\u200b", `'/tmp/\u200b'`},
		{"/tmp/\U000e0001", `'/tmp/\U000e0001'`},
		{"/tmp/\ue000", `'/tmp/\ue000'`},
		{"/tmp/\u00e9\u3042", "'/tmp/\u00e9\u3042'"},
		{"/tmp/\u0378", `'/tmp/\u0378'`},
		{"/tmp/st\u0c5cate", `'/tmp/st\u0c5cate'`}, // unassigned in CPython 3.14's Unicode 16.0.0
		{"/tmp/\u1680x", `'/tmp/\u1680x'`},
	} {
		if got := PathRepr(c.name); got != c.want {
			t.Errorf("PathRepr(%q) = %s, want %s", c.name, got, c.want)
		}
	}
	err := &os.PathError{Op: "stat", Path: "/tmp/a\u00a0\xffb", Err: syscall.EACCES}
	if got := StoredOSError(err); got != `PermissionError: [Errno 13] Permission denied: '/tmp/a\xa0\udcffb'` {
		t.Fatal(got)
	}
	link := &os.LinkError{Op: "rename", Old: "/tmp/\u2028", New: "/tmp/\xff", Err: syscall.ENOENT}
	if got := StoredOSErrorText(link); got != `[Errno 2] No such file or directory: '/tmp/\u2028' -> '/tmp/\udcff'` {
		t.Fatal(got)
	}
}
