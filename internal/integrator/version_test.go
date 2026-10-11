package integrator

import (
	"errors"
	"testing"
)

func conflict(ours, theirs string) string {
	return "{\n  \"name\": \"crw\",\n<<<<<<< HEAD\n" + ours + "\n=======\n" + theirs + "\n>>>>>>> abc123\n  \"description\": \"x\"\n}\n"
}

func TestResolveVersionConflict(t *testing.T) {
	ok := func(name, in, want string) {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveVersionConflict([]byte(in))
			if err != nil || string(got) != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
	bad := func(name, in string) {
		t.Run(name, func(t *testing.T) {
			if got, err := ResolveVersionConflict([]byte(in)); !errors.Is(err, ErrNotVersionOnly) || got != nil {
				t.Fatalf("got %q, %v; want the refusal", got, err)
			}
		})
	}
	ok("keeps the current side", conflict(`  "version": "v-2-a",`, `  "version": "v-2-b",`),
		"{\n  \"name\": \"crw\",\n  \"version\": \"v-2-a\",\n  \"description\": \"x\"\n}\n")
	ok("version line without a comma", conflict(`  "version": "1"`, `"version":"2"`),
		"{\n  \"name\": \"crw\",\n  \"version\": \"1\"\n  \"description\": \"x\"\n}\n")
	ok("diff3 base section", "<<<<<<< HEAD\n  \"version\": \"a\",\n||||||| base\n  \"version\": \"0\",\n=======\n  \"version\": \"b\",\n>>>>>>> x\n",
		"  \"version\": \"a\",\n")
	ok("two blocks", conflict(`  "version": "a",`, `  "version": "b",`)+conflict(`  "version": "c",`, `  "version": "d",`),
		"{\n  \"name\": \"crw\",\n  \"version\": \"a\",\n  \"description\": \"x\"\n}\n{\n  \"name\": \"crw\",\n  \"version\": \"c\",\n  \"description\": \"x\"\n}\n")
	ok("CRLF kept", "<<<<<<< HEAD\r\n  \"version\": \"a\",\r\n=======\r\n  \"version\": \"b\",\r\n>>>>>>> x\r\nrest\r\n", "  \"version\": \"a\",\r\nrest\r\n")

	bad("no conflict block", "{\n  \"version\": \"1\"\n}\n")
	bad("another key", conflict(`  "name": "a",`, `  "name": "b",`))
	bad("one side is another key", conflict(`  "version": "a",`, `  "name": "b",`))
	bad("a side with two lines", conflict("  \"version\": \"a\",\n  \"x\": 1,", `  "version": "b",`))
	bad("an empty side", "<<<<<<< HEAD\n=======\n  \"version\": \"b\",\n>>>>>>> x\n")
	bad("unterminated block", "<<<<<<< HEAD\n  \"version\": \"a\",\n=======\n  \"version\": \"b\",\n")
	bad("one good block and one bad", conflict(`  "version": "a",`, `  "version": "b",`)+conflict(`  "x": 1,`, `  "x": 2,`))
	bad("a version-looking key", conflict(`  "min_version": "a",`, `  "min_version": "b",`))
}
