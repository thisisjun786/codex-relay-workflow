// Package inbox is decision 25's durable takeover inbox on the Go side: the canonical entry
// bytes a queued receipt or acknowledgment is published as, the publication protocol that
// acknowledges it only once durable, and the owner's replay that applies it once through the
// existing handler (docs/port/cutover.md Inbox and Wire format). The retained Python fence
// (codex_session_relay/inbox.py) is the reference: both runtimes write and read the same
// files under S/takeover-inbox/.
package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// stableKeys names, per command, the argument whose value is the operation ID's stable key;
// "" is the first 32 hex digits of the payload digest (inbox.py REPLAY_KEYS). supervisor-read
// is not queueable: it is only replayed, for entries the preceding fence build accepted.
var stableKeys = map[string]string{"emit": "", "ack": "event", "fault-notification-ack": "notification", "supervisor-read": "message"}

// defaults are the argparse defaults other than None of the replayable commands' options, by
// dest (cli.py build_parser); a value equal to its default is omitted from an entry.
var defaults = map[string]map[string]any{
	"emit": {"attempt": big.NewInt(1), "turn_status": "inProgress", "no_enqueue": false},
}

// Queueable reports whether command queues on a queueable ownership refusal (inbox.py
// COMMAND_KEYS): only receipt ingestion and acknowledgments do.
func Queueable(command string) bool {
	return command == "emit" || command == "ack" || command == "fault-notification-ack"
}

// maxID is the longest encoded operation ID, which is also the entry's file name.
const maxID = 200

// Entry is one decision-25 request: its canonical bytes and what they carry.
type Entry struct {
	Raw     []byte
	ID      string
	Digest  string
	Command string
}

// UsageError is an ingress rejection decided before any I/O (an overlength identifier, an
// argument that is not valid UTF-8): exit 4 {"error": "usage", "detail": ...}.
type UsageError struct{ Detail string }

func (e *UsageError) Error() string { return e.Detail }

// Envelope is inbox.envelope for command and its own arguments (the argv after the command
// name, as argparse parsed it): the arguments keyed by argparse dest with defaults omitted,
// their payload digest, the percent-encoded operation ID and the canonical entry bytes.
func Envelope(command string, argv []string) (Entry, error) {
	if _, known := stableKeys[command]; !known {
		return Entry{}, fmt.Errorf("not an inbox command: %s", command)
	}
	parsed := argparse.Parse(command, argv)
	if parsed.Message != "" || parsed.Help {
		return Entry{}, fmt.Errorf("unparsed %s arguments: %s", command, parsed.Message)
	}
	arguments := map[string]any{}
	for _, action := range argparse.Specs[command].Actions {
		if action.Dest == "help" || len(action.Flags) == 0 {
			continue
		}
		name := strings.TrimPrefix(action.Flags[len(action.Flags)-1], "--")
		if !parsed.Given[name] {
			continue // the default, which is omitted
		}
		values := parsed.Values[name]
		var value any
		switch {
		case action.Kind == "_AppendAction":
			value = slices.Clone(values)
		case action.Kind == "_StoreTrueAction":
			value = true
		case action.Kind == "_StoreFalseAction":
			value = false
		case action.Type == "int":
			value = parsed.Numbers[name].(*big.Int)
		default:
			value = values[len(values)-1]
		}
		if !isDefault(command, action, value) {
			arguments[action.Dest] = value
		}
	}
	return envelope(command, arguments)
}

// isDefault is Python's `value == action.default` for one parsed option value.
func isDefault(command string, action argparse.Action, value any) bool {
	def, has := defaults[command][action.Dest]
	if !has {
		// None, which no parsed value equals; store_true's default is False, store_false's True.
		switch action.Kind {
		case "_StoreTrueAction":
			return value == false
		case "_StoreFalseAction":
			return value == true
		}
		return false
	}
	switch d := def.(type) {
	case *big.Int:
		n, ok := value.(*big.Int)
		return ok && n.Cmp(d) == 0
	default:
		return value == def
	}
}

// envelope is the second half of inbox.envelope: digest, identifier and entry bytes of
// already-built arguments.
func envelope(command string, arguments map[string]any) (Entry, error) {
	// canonical(arguments) encodes to UTF-8 first; Python's surrogate-escaped argv fails there.
	if detail := unencodable(arguments); detail != "" {
		return Entry{}, &UsageError{Detail: detail}
	}
	sum := sha256.Sum256(canonical(arguments))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	key := digest[7:39]
	if dest := stableKeys[command]; dest != "" {
		key = fmt.Sprint(pyStr(arguments[dest]))
	}
	id := encodeID(command + "." + key)
	if len(id) > maxID {
		return Entry{}, &UsageError{Detail: "inbox operation ID exceeds 200 encoded characters"}
	}
	raw := canonical(map[string]any{"inboxVersion": 1, "operationId": id, "command": command, "arguments": arguments, "payloadDigest": digest})
	return Entry{Raw: raw, ID: id, Digest: digest, Command: command}, nil
}

// pyStr is str() of a stable key value: every stable key is a string option.
func pyStr(value any) any {
	if n, ok := value.(*big.Int); ok {
		return n.String()
	}
	return value
}

// encodeID percent-encodes every UTF-8 byte outside [A-Za-z0-9._-] as uppercase %XX.
func encodeID(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonical is inbox.canonical: sort_keys, compact separators, ensure_ascii=False, UTF-8.
// Integers are *big.Int, json.Number or int; every string is valid UTF-8 by the time it gets here.
func canonical(value any) []byte {
	return []byte(evidence.Dumps(jsonValue(value), true, true, false))
}

// jsonValue turns the arbitrary-precision integers an entry carries into the JSON numbers the
// evidence dumper writes verbatim.
func jsonValue(value any) any {
	switch v := value.(type) {
	case *big.Int:
		return json.Number(v.String())
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = jsonValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = jsonValue(item)
		}
		return out
	}
	return value
}

// unencodable is the UnicodeEncodeError str() Python raises when canonical(arguments), whose
// argv strings were decoded with surrogateescape, is encoded to UTF-8; "" when every string
// is valid UTF-8. Each byte outside a valid sequence is one lone surrogate U+DC80..U+DCFF, and
// the position counts code points of the ensure_ascii=False JSON text.
func unencodable(arguments map[string]any) string {
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	position := 1 // the opening brace
	var first rune
	start, end := -1, -1
	scan := func(s string) bool {
		position++ // opening quote
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			i += size
			if r == utf8.RuneError && size == 1 {
				if start < 0 {
					start, first = position, 0xdc00+rune(s[i-1])
				}
				end = position + 1
				position++
				continue
			}
			if start >= 0 {
				return true // the run of unencodable characters ended
			}
			switch {
			case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
				position += 2
			case r < 0x20:
				position += 6
			default:
				position++
			}
		}
		if start >= 0 {
			return true
		}
		position++ // closing quote
		return false
	}
	for i, key := range keys {
		if i > 0 {
			position++ // comma
		}
		position += len(key) + 3 // an ASCII dest, its quotes and the colon
		var stop bool
		switch v := arguments[key].(type) {
		case string:
			stop = scan(v)
		case []string:
			position++ // opening bracket
			for j, item := range v {
				if j > 0 {
					position++
				}
				if stop = scan(item); stop {
					break
				}
			}
			position++
		case *big.Int:
			position += len(v.String())
		case bool:
			position += len(fmt.Sprint(v))
		}
		if stop {
			break
		}
	}
	if start < 0 {
		return ""
	}
	if end-start == 1 {
		return fmt.Sprintf(`'utf-8' codec can't encode character '\u%04x' in position %d: surrogates not allowed`, first, start)
	}
	return fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, end-1)
}
