package hook

import (
	"reflect"
	"testing"
)

func TestShellIRWriteDests(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"echo x > out.txt", []string{"out.txt"}},
		{"echo x >> out.txt", []string{"out.txt"}},
		{"cat < in.txt", nil},
		{"echo x > \"$F\"", []string{shellIRUnknownDest}},
		{"tee a.txt b.txt", []string{"a.txt", "b.txt"}},
		{"cp src dst", []string{"dst"}},
		{"mv src dst", []string{"dst"}},
		{"dd if=x of=y bs=1", []string{"y"}},
		{"sort -o out in", []string{"out"}},
		{"sed -i 's/a/b/' f1 f2", []string{"f1", "f2"}},
		{"sed -i -e 's/a/b/' f1", []string{"f1"}},
		{"sed 's/a/b/' f1", nil},
	}
	for _, c := range cases {
		got, ok := shellIRWriteDests(c.cmd, "/work", nil)
		if !ok {
			t.Fatalf("%q: unreadable", c.cmd)
		}
		if !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("%q: dests %q, want %q", c.cmd, got, c.want)
		}
	}
}
