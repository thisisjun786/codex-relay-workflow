package shellir

import "testing"

func TestDirectoryRequiresCDSuccess(t *testing.T) {
	for _, tc := range []struct {
		command, knownPath string
	}{
		{"rm x", "/work"},
		{"cd /outside; rm x", ""},
		{"cd /outside\nrm x", ""},
		{"cd /outside || true; rm x", ""},
		{"cd /outside || rm x", ""},
		{"cd /outside && true; rm x", ""},
		{"cd /outside && true && true; rm x", ""},
		{"cd /outside; cd child && rm x", ""},
		{"! cd /outside && rm x", ""},
		{"{ cd /outside; true; } && rm x", ""},
		{"f() { cd /outside; true; }; f && rm x", ""},
		{"bash -c 'cd /outside; rm x'", ""},
		{"eval 'cd /outside; rm x'", ""},
		{"(cd /outside; rm x)", ""},
		{"cd /outside || true && rm x", ""},
		{"(cd /outside); rm x", "/work"},
		{"cd /outside && rm x", "/outside"},
		{"cd /outside && true && rm x", "/outside"},
		{"cd /outside && cd child && rm x", "/outside/child"},
		{"cd /outside && (rm x)", "/outside"},
		{"cd /outside; cd /another && rm x", "/another"},
		{"cd /work; rm x", "/work"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			r, err := AnalyzeEnvProvenDirectory(tc.command, "/work", nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range r.Execs {
				if e.Name != "rm" {
					continue
				}
				found = true
				if e.Dir.Known != (tc.knownPath != "") || (e.Dir.Known && e.Dir.Path != tc.knownPath) {
					t.Errorf("directory %+v, want proven path %q", e.Dir, tc.knownPath)
				}
			}
			if !found {
				t.Fatal("no rm record")
			}
		})
	}
}
