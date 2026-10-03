package agy

import (
	"os"
	"path/filepath"
)

// callDir is the directory of one call. work is the empty directory agy runs in; the schema file and agy's log sit beside it, outside what agy can list.
type callDir struct{ root, work, schema, log string }

// newCallDir makes the directory under parent, with the schema file when there is a schema; it leaves nothing behind when it fails.
func newCallDir(parent string, schema []byte) (callDir, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return callDir{}, err
	}
	root, err := os.MkdirTemp(parent, "crw-agy-")
	if err != nil {
		return callDir{}, err
	}
	d := callDir{root: root, work: filepath.Join(root, "work"), log: filepath.Join(root, "agy.log")}
	err = os.Mkdir(d.work, 0o700)
	if err == nil && len(schema) > 0 {
		d.schema = filepath.Join(root, "schema.json")
		err = os.WriteFile(d.schema, schema, 0o600)
	}
	if err != nil {
		_ = os.RemoveAll(root)
		return callDir{}, err
	}
	return d, nil
}

// args is the whole argument list of the call: the prompt is not among it, it goes on stdin.
func (d callDir) args(model string) []string {
	args := []string{"--model", model, "--output-format", "json"}
	if d.schema != "" {
		args = append(args, "--json-schema", d.schema)
	}
	return append(args, "--disable-slash-commands", "--log-file", d.log)
}
