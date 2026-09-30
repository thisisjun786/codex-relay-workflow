package pyoracle

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntern_then_Expand_gives_back_every_answer_exactly(t *testing.T) {
	dump := `{"rows": [{"id": 1, "path": "/state/a"}, {"id": 2}]}` + "\n"
	for _, answer := range []string{
		"", "}", "\n", "}}\n\n", "no break at all", "trailing text after }",
		strings.Repeat(dump, 50), strings.Repeat(dump, 3) + `{"rows": [{"id": 3}]}` + strings.Repeat(dump, 3),
		"café }\n }", "\xff\x00}", "holds the \x1e separator}",
	} {
		encoded := Intern([]byte(answer))
		got, err := Expand(encoded)
		if err != nil {
			t.Fatalf("%q: %v", answer, err)
		}
		if string(got) != answer {
			t.Fatalf("%q came back as %q", answer, got)
		}
	}
}

func TestIntern_stores_a_repeated_dump_once(t *testing.T) {
	var rows strings.Builder
	for id := range 200 {
		fmt.Fprintf(&rows, `{"table": "t", "id": %d}, `, id)
	}
	answer := strings.Repeat(rows.String()+"\n", 100)
	encoded := Intern([]byte(answer))
	if len(encoded)*50 > len(answer) {
		t.Fatalf("%d bytes encoded as %d", len(answer), len(encoded))
	}
}

func TestAnswerInterned_keeps_a_small_answer_readable(t *testing.T) {
	inFreshDirectory(t)
	t.Setenv(ModeEnv, "record")
	path := filepath.Join(Directory, fileName(t.Name()))
	AnswerInterned(t, "k", func() ([]byte, error) { return []byte("{\"a\": 1}\n{\"a\": 1}\n"), nil })
	forget()
	data, err := readRecording(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "interned") {
		t.Fatalf("a small answer was encoded: %s", data)
	}
}

func TestIntern_leaves_an_answer_it_cannot_encode_as_it_is(t *testing.T) {
	for _, answer := range []string{"\xff\x00}", "a}\x1eb}"} {
		if got := Intern([]byte(answer)); string(got) != answer {
			t.Fatalf("%q was encoded as %q", answer, got)
		}
	}
	if got, err := Expand([]byte("plain answer")); err != nil || string(got) != "plain answer" {
		t.Fatalf("a plain answer expanded to %q, %v", got, err)
	}
	for _, broken := range []string{internedPrefix, internedPrefix + "0+2\na}\x1e", internedPrefix + "0+1\na}", internedPrefix + "x\na}\x1e", internedPrefix + "0+1*0\na}\x1e", internedPrefix + "0+1*x\na}\x1e"} {
		if _, err := Expand([]byte(broken)); err == nil {
			t.Fatalf("%q expanded", broken)
		}
	}
}

func TestAnswerInterned_records_the_encoding_and_replays_the_answer_with_run_paths_put_back(t *testing.T) {
	inFreshDirectory(t)
	answer := func(run string) string {
		return `{"db": "` + run + `/relay.sqlite3"}` + strings.Repeat(`, {"row": 1}, {"row": 2}`+"\n", internAbove)
	}
	var path string
	{
		t.Setenv(ModeEnv, "record")
		path = filepath.Join(Directory, fileName(t.Name()))
		got := AnswerInterned(t, "k", func() ([]byte, error) { return []byte(answer("/tmp/run-1")), nil },
			Substitute("/tmp/run-1", "<TMP>"))
		if string(got) != answer("/tmp/run-1") {
			t.Fatalf("record returned %q", got)
		}
	}
	forget()
	data, err := readRecording(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/tmp/run-1") || !strings.Contains(string(data), "<TMP>") || len(data) > 1024 {
		t.Fatalf("the recording is not the encoded answer with the run path replaced: %s", clip(data))
	}
	{
		t.Setenv(ModeEnv, "check")
		AnswerInterned(t, "k", func() ([]byte, error) { return []byte(answer("/tmp/run-2")), nil },
			Substitute("/tmp/run-2", "<TMP>"))
		message := fatalOf(t, func(tb testing.TB) {
			AnswerInterned(tb, "k", func() ([]byte, error) { return []byte(answer("/tmp/run-2") + "}"), nil },
				Substitute("/tmp/run-2", "<TMP>"))
		})
		if !strings.Contains(message, "differently") {
			t.Fatalf("a changed answer passed check: %q", message)
		}
	}
	{
		t.Setenv(ModeEnv, "")
		got := AnswerInterned(t, "k", func() ([]byte, error) { return nil, errors.New("replay must not capture") },
			Substitute("/tmp/run-a-longer-path-3", "<TMP>"))
		if string(got) != answer("/tmp/run-a-longer-path-3") {
			t.Fatalf("replay returned %q", got)
		}
	}
}
