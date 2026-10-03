package pipeline

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// Validate the exact schema sent to the runner before decoding typed values.
// json.Unmarshal rejects trailing JSON. Like the core, duplicate keys are last-wins.
func parse(data []byte, schema string, out any) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var contract jsonschema.Schema
	if err := json.Unmarshal([]byte(schema), &contract); err != nil {
		return err
	}
	resolved, err := contract.Resolve(nil)
	if err != nil {
		return err
	}
	if err = resolved.Validate(raw); err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
