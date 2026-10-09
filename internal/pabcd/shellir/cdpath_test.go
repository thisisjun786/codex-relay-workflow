package shellir

import "testing"

// TestCdUnderCDPATH: with CDPATH assigned a bare name may land in a CDPATH directory the text does not show, so the directory is
// unknown; an absolute or ./ target is never searched there and resolves as without CDPATH (CRW-875).
func TestCdUnderCDPATH(t *testing.T) {
	cases := []struct {
		name, cmd string
		path      string // the known directory of the last record, "" when it must be unknown
	}{
		{"assignment alone moves nothing", "CDPATH=/x; rm a", "/work"},
		{"absolute target", "CDPATH=/x; cd /abs/sub; rm a", "/abs/sub"},
		{"dot-slash target", "CDPATH=/x; cd ./sub; rm a", "/work/sub"},
		{"exported", "export CDPATH=/x:.; cd ./sub; rm a", "/work/sub"},
		{"dot-slash then absolute", "CDPATH=/x; cd ./sub; cd /abs; rm a", "/abs"},
		{"bare name is searched", "CDPATH=/x; cd sub; rm a", ""},
		{"bare name after a known cd", "CDPATH=/x; cd /abs; cd sub; rm a", ""},
		{"dot is not a ./ target", "CDPATH=/x; cd .; rm a", ""},
		{"dot-dot target", "CDPATH=/x; cd ../sub; rm a", ""},
		{"dot-slash under an unknown directory stays unknown", "CDPATH=/x; cd sub; cd ./inner; rm a", ""},
		{"control: no CDPATH, bare name", "cd sub; rm a", "/work/sub"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Analyze(c.cmd, "/work")
			if err != nil {
				t.Fatal(err)
			}
			got := r.Execs[len(r.Execs)-1].Dir
			if c.path == "" {
				if got.Known {
					t.Fatalf("%q: dir %+v, want unknown", c.cmd, got)
				}
				return
			}
			if !got.Known || got.Path != c.path {
				t.Fatalf("%q: dir %+v, want known %s", c.cmd, got, c.path)
			}
		})
	}
}
