package script

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Limits of the strict structured-output rules (engine.agentPrimitive.schemaStrictness): the documented depth
// and size ceilings of the output schemas Codex sends as a strict json_schema.
const (
	MaxSchemaDepth      = 10
	MaxSchemaProperties = 5000
	MaxSchemaEnumValues = 1000
)

// strictKeywords are the keywords a strict schema may use: structure, composition by anyOf and local references,
// and the documented string, number and array constraints. Anything else (oneOf, allOf, not, if/then/else,
// patternProperties, unevaluatedProperties, title, default ...) is refused.
var strictKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"enum": true, "const": true, "anyOf": true, "$defs": true, "$ref": true, "description": true,
	"pattern": true, "format": true, "minimum": true, "maximum": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "multipleOf": true, "minItems": true, "maxItems": true,
}

var schemaTypes = map[string]bool{"object": true, "array": true, "string": true, "number": true, "integer": true, "boolean": true, "null": true}

// LintStrictSchema checks a JSON Schema (decoded JSON) against the strict structured-output rules and returns
// every violation, each prefixed by the JSON pointer of the node it is on. An empty result means the schema may
// be sent as outputSchema:
//
//   - the root is an object (type "object", no root-level anyOf);
//   - every object lists additionalProperties false and every one of its properties in required (an optional
//     field is a required nullable union such as type ["string","null"]);
//   - only the supported keywords appear;
//   - $ref points at an existing local #/$defs entry;
//   - depth, property count and enum size stay under the documented limits.
func LintStrictSchema(schema any) []string {
	l := &schemaLinter{}
	root, ok := schema.(map[string]any)
	if !ok {
		return []string{"/: the schema must be a JSON object"}
	}
	if !typeIncludes(root["type"], "object") || isUnion(root["type"]) {
		l.add("", `the root must have type "object"`)
	}
	if _, has := root["anyOf"]; has {
		l.add("", "the root must not be an anyOf")
	}
	l.root = root
	l.node("", root, 1)
	if l.props > MaxSchemaProperties {
		l.add("", "%d properties exceed the limit of %d", l.props, MaxSchemaProperties)
	}
	if l.enums > MaxSchemaEnumValues {
		l.add("", "%d enum values exceed the limit of %d", l.enums, MaxSchemaEnumValues)
	}
	return l.errs
}

type schemaLinter struct {
	root         map[string]any
	errs         []string
	props, enums int
}

func (l *schemaLinter) add(ptr, format string, args ...any) {
	if ptr == "" {
		ptr = "/"
	}
	l.errs = append(l.errs, ptr+": "+fmt.Sprintf(format, args...))
}

func typeList(t any) []string {
	switch v := t.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	}
	return nil
}

func typeIncludes(t any, want string) bool {
	for _, s := range typeList(t) {
		if s == want {
			return true
		}
	}
	return false
}

func isUnion(t any) bool { return len(typeList(t)) > 1 }

func (l *schemaLinter) node(ptr string, n map[string]any, depth int) {
	if depth > MaxSchemaDepth {
		l.add(ptr, "nesting deeper than %d levels", MaxSchemaDepth)
		return
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strictKeywords[k] {
			l.add(ptr, "keyword %q is not supported in a strict structured-output schema", k)
		}
	}
	if t, has := n["type"]; has {
		ts := typeList(t)
		if len(ts) == 0 {
			l.add(ptr, "type must be a type name or a non-empty list of type names")
		}
		for _, s := range ts {
			if !schemaTypes[s] {
				l.add(ptr, "unknown type %q", s)
			}
		}
	} else if _, a := n["anyOf"]; !a {
		if _, r := n["$ref"]; !r {
			if _, c := n["const"]; !c {
				if _, e := n["enum"]; !e {
					l.add(ptr, "the node has no type, anyOf, $ref, const or enum")
				}
			}
		}
	}
	if v, has := n["enum"]; has {
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			l.add(ptr, "enum must be a non-empty list")
		}
		l.enums += len(list)
	}
	if v, has := n["$ref"]; has {
		ref, _ := v.(string)
		if name, ok := strings.CutPrefix(ref, "#/$defs/"); !ok || !l.hasDef(name) {
			l.add(ptr, "$ref %q must name an entry of the root $defs", ref)
		}
	}
	if v, has := n["anyOf"]; has {
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			l.add(ptr, "anyOf must be a non-empty list of schemas")
		}
		for i, b := range list {
			if m, ok := b.(map[string]any); ok {
				l.node(fmt.Sprintf("%s/anyOf/%d", ptr, i), m, depth+1)
			} else {
				l.add(ptr, "anyOf/%d is not a schema", i)
			}
		}
	}
	if v, has := n["$defs"]; has {
		defs, ok := v.(map[string]any)
		if !ok {
			l.add(ptr, "$defs must be an object")
		}
		names := make([]string, 0, len(defs))
		for k := range defs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if m, ok := defs[k].(map[string]any); ok {
				l.node(ptr+"/$defs/"+k, m, depth+1)
			} else {
				l.add(ptr, "$defs/%s is not a schema", k)
			}
		}
	}
	if v, has := n["items"]; has {
		if m, ok := v.(map[string]any); ok {
			l.node(ptr+"/items", m, depth+1)
		} else {
			l.add(ptr, "items must be one schema")
		}
	} else if typeIncludes(n["type"], "array") {
		l.add(ptr, `an array schema needs "items"`)
	}
	_, hasProps := n["properties"]
	isObj := typeIncludes(n["type"], "object") || hasProps
	if isObj {
		l.object(ptr, n, depth)
	} else if _, has := n["required"]; has {
		l.add(ptr, "required without properties")
	}
}

func (l *schemaLinter) object(ptr string, n map[string]any, depth int) {
	if ap, has := n["additionalProperties"]; !has || ap != false {
		l.add(ptr, "an object needs additionalProperties: false")
	}
	props, _ := n["properties"].(map[string]any)
	if n["properties"] != nil && props == nil {
		l.add(ptr, "properties must be an object")
	}
	var req []string
	if r, has := n["required"]; has {
		list, ok := r.([]any)
		if !ok {
			l.add(ptr, "required must be a list of property names")
		}
		for _, e := range list {
			if s, ok := e.(string); ok {
				req = append(req, s)
			} else {
				l.add(ptr, "required holds a non-string entry")
			}
		}
	}
	reqSet := map[string]bool{}
	for _, r := range req {
		reqSet[r] = true
		if _, ok := props[r]; !ok {
			l.add(ptr, "required names %q, which is not a property", r)
		}
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	l.props += len(names)
	for _, k := range names {
		if !reqSet[k] {
			l.add(ptr, "property %q is not in required (an optional field is a required nullable union)", k)
		}
		if m, ok := props[k].(map[string]any); ok {
			l.node(ptr+"/properties/"+k, m, depth+1)
		} else {
			l.add(ptr, "property %q is not a schema", k)
		}
	}
}

func (l *schemaLinter) hasDef(name string) bool {
	defs, _ := l.root["$defs"].(map[string]any)
	_, ok := defs[name]
	return ok
}

// LintStrictSchemaJSON is LintStrictSchema over JSON text.
func LintStrictSchemaJSON(data []byte) []string {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return []string{"/: not JSON: " + err.Error()}
	}
	return LintStrictSchema(v)
}
