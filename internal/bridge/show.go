package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// show is a value as a message names it: a string Go-quoted, anything else as its compact JSON
// text, '<', '>' and '&' as themselves.
func show(value any) string {
	if s, ok := value.(string); ok {
		return strconv.Quote(s)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// findingText is "{code}: {field} returned {returned}, expected {expected}".
func findingText(code, field string, returned, expected any) string {
	return fmt.Sprintf("%s: %s returned %s, expected %s", code, field, show(returned), show(expected))
}
