package evidence

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SchemaError is a document that does not validate, naming the first place
// it went wrong as a JSON Pointer. The path is what a failure is reported
// by: a session told that its output is "invalid" cannot fix it, and a
// session told that `/findings/0/severity` is not one of `blocking`, `note`
// can.
type SchemaError struct {
	// Path is the JSON Pointer of the violating location, empty for the
	// document itself.
	Path string
	// Reason says what is wrong there.
	Reason string
}

func (e *SchemaError) Error() string {
	if e.Path == "" {
		return "the document " + e.Reason
	}
	return e.Path + " " + e.Reason
}

// UnsupportedKeywordError is a schema this validator will not pretend to
// understand. A validator that ignores the keywords it does not implement
// reports documents as valid that are not, which would make `structured`
// evidence a claim rather than a fact — so an unknown keyword fails the
// schema at the point of use instead.
type UnsupportedKeywordError struct {
	// Path is the JSON Pointer, within the schema, of the subschema
	// carrying the keyword.
	Path string
	// Keyword is the keyword itself.
	Keyword string
}

func (e *UnsupportedKeywordError) Error() string {
	where := e.Path
	if where == "" {
		where = "the schema"
	}
	return fmt.Sprintf("%s: %q is not a keyword this validator implements", where, e.Keyword)
}

// keywords is the subset of JSON Schema the mill validates against: the
// structural and enumerated constraints an output schema or a check's stdout
// contract is written with. Annotations are accepted and ignored; anything
// else is an [UnsupportedKeywordError].
//
// The subset is deliberate rather than a stage: a workflow's schema is
// written by the same people who write the workflow, and a keyword that has
// to be added is one line here.
var keywords = map[string]bool{
	// Structural.
	"type": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true,
	// Enumerated and scalar constraints.
	"enum": true, "const": true, "pattern": true,
	"minLength": true, "maxLength": true,
	"minimum": true, "maximum": true,
	"minItems": true, "maxItems": true,
	// Annotations, which constrain nothing.
	"$schema": true, "$id": true, "title": true, "description": true,
	"default": true, "examples": true, "deprecated": true,
}

// ValidateJSON checks a document against a JSON Schema and reports the first
// violating path. The document is what `encoding/json` yields — maps,
// slices, strings, float64, bool, nil — which is also what a harness's
// structured output arrives as.
//
// Traversal is deterministic: a node's own constraints first, then its
// required properties in the order the schema lists them, then its
// properties in sorted order, then array elements by index. Two runs over
// the same document therefore name the same first violation.
func ValidateJSON(schema string, document any) error {
	if strings.TrimSpace(schema) == "" {
		return &SchemaError{Reason: "was validated against no schema"}
	}
	var parsed any
	if err := json.Unmarshal([]byte(schema), &parsed); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return validate("", parsed, normalize(document))
}

// normalize turns whatever Go shape a caller holds into the shapes
// encoding/json produces, so that a document built in a spec and one decoded
// off a pipe validate identically.
func normalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalize(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = normalize(item)
		}
		return out
	case json.Number:
		f, err := typed.Float64()
		if err != nil {
			return typed.String()
		}
		return f
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case float32:
		return float64(typed)
	default:
		return value
	}
}

//nolint:gocyclo // one branch per keyword; splitting it hides the order violations are reported in
func validate(path string, schema, document any) error {
	switch typed := schema.(type) {
	case bool:
		// The boolean schemas: true accepts anything, false accepts nothing.
		if !typed {
			return &SchemaError{Path: path, Reason: "is not permitted here"}
		}
		return nil
	case map[string]any:
		for _, keyword := range sortedKeys(typed) {
			if !keywords[keyword] {
				return &UnsupportedKeywordError{Path: path, Keyword: keyword}
			}
		}
		if err := checkType(path, typed, document); err != nil {
			return err
		}
		if err := checkConst(path, typed, document); err != nil {
			return err
		}
		if err := checkEnum(path, typed, document); err != nil {
			return err
		}
		if err := checkString(path, typed, document); err != nil {
			return err
		}
		if err := checkNumber(path, typed, document); err != nil {
			return err
		}
		if err := checkObject(path, typed, document); err != nil {
			return err
		}
		return checkArray(path, typed, document)
	default:
		return fmt.Errorf("schema: %s: a subschema is an object or a boolean", pathOrRoot(path))
	}
}

func checkType(path string, schema map[string]any, document any) error {
	declared, ok := schema["type"]
	if !ok {
		return nil
	}
	var names []string
	switch typed := declared.(type) {
	case string:
		names = []string{typed}
	case []any:
		for _, name := range typed {
			text, ok := name.(string)
			if !ok {
				return fmt.Errorf("schema: %s: type names are strings", pathOrRoot(path))
			}
			names = append(names, text)
		}
	default:
		return fmt.Errorf("schema: %s: type is a string or a list of strings", pathOrRoot(path))
	}
	for _, name := range names {
		if isType(name, document) {
			return nil
		}
	}
	return &SchemaError{Path: path, Reason: fmt.Sprintf("is %s, not %s", typeOf(document), strings.Join(names, " or "))}
}

func isType(name string, document any) bool {
	switch name {
	case "object":
		_, ok := document.(map[string]any)
		return ok
	case "array":
		_, ok := document.([]any)
		return ok
	case "string":
		_, ok := document.(string)
		return ok
	case "boolean":
		_, ok := document.(bool)
		return ok
	case "number":
		_, ok := document.(float64)
		return ok
	case "integer":
		number, ok := document.(float64)
		return ok && number == float64(int64(number))
	case "null":
		return document == nil
	default:
		return false
	}
}

// typeOf is the document's own type, for the error that says what was found.
func typeOf(document any) string {
	switch typed := document.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		if typed == float64(int64(typed)) {
			return "an integer"
		}
		return "a number"
	default:
		return fmt.Sprintf("%T", document)
	}
}

func checkConst(path string, schema map[string]any, document any) error {
	want, ok := schema["const"]
	if !ok {
		return nil
	}
	if !equal(normalize(want), document) {
		return &SchemaError{Path: path, Reason: fmt.Sprintf("is not %s", render(want))}
	}
	return nil
}

func checkEnum(path string, schema map[string]any, document any) error {
	declared, ok := schema["enum"]
	if !ok {
		return nil
	}
	values, ok := declared.([]any)
	if !ok {
		return fmt.Errorf("schema: %s: enum is a list", pathOrRoot(path))
	}
	rendered := make([]string, 0, len(values))
	for _, value := range values {
		if equal(normalize(value), document) {
			return nil
		}
		rendered = append(rendered, render(value))
	}
	return &SchemaError{Path: path, Reason: "is not one of " + strings.Join(rendered, ", ")}
}

func checkString(path string, schema map[string]any, document any) error {
	text, ok := document.(string)
	if !ok {
		return nil
	}
	if limit, ok := schema["minLength"]; ok {
		if n, err := count(path, "minLength", limit); err != nil {
			return err
		} else if len([]rune(text)) < n {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("is shorter than %d characters", n)}
		}
	}
	if limit, ok := schema["maxLength"]; ok {
		if n, err := count(path, "maxLength", limit); err != nil {
			return err
		} else if len([]rune(text)) > n {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("is longer than %d characters", n)}
		}
	}
	if declared, ok := schema["pattern"]; ok {
		expression, ok := declared.(string)
		if !ok {
			return fmt.Errorf("schema: %s: pattern is a string", pathOrRoot(path))
		}
		pattern, err := regexp.Compile(expression)
		if err != nil {
			return fmt.Errorf("schema: %s: pattern: %w", pathOrRoot(path), err)
		}
		if !pattern.MatchString(text) {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("does not match %s", expression)}
		}
	}
	return nil
}

func checkNumber(path string, schema map[string]any, document any) error {
	number, ok := document.(float64)
	if !ok {
		return nil
	}
	for _, bound := range []struct {
		keyword string
		below   bool
	}{{"minimum", true}, {"maximum", false}} {
		declared, ok := schema[bound.keyword]
		if !ok {
			continue
		}
		limit, ok := normalize(declared).(float64)
		if !ok {
			return fmt.Errorf("schema: %s: %s is a number", pathOrRoot(path), bound.keyword)
		}
		if bound.below && number < limit {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("is below %s", render(declared))}
		}
		if !bound.below && number > limit {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("is above %s", render(declared))}
		}
	}
	return nil
}

func checkObject(path string, schema map[string]any, document any) error {
	object, ok := document.(map[string]any)
	if !ok {
		return nil
	}
	properties := map[string]any{}
	if declared, ok := schema["properties"]; ok {
		if properties, ok = declared.(map[string]any); !ok {
			return fmt.Errorf("schema: %s: properties is an object", pathOrRoot(path))
		}
	}
	// Required first, and in the order the schema lists them: a missing
	// property is the violation a reader is looking for, and the schema's
	// own order is the one its author reasons in.
	if declared, ok := schema["required"]; ok {
		names, ok := declared.([]any)
		if !ok {
			return fmt.Errorf("schema: %s: required is a list", pathOrRoot(path))
		}
		for _, name := range names {
			text, ok := name.(string)
			if !ok {
				return fmt.Errorf("schema: %s: required names properties", pathOrRoot(path))
			}
			if _, present := object[text]; !present {
				return &SchemaError{Path: child(path, text), Reason: "is required and missing"}
			}
		}
	}
	for _, name := range sortedKeys(properties) {
		value, present := object[name]
		if !present {
			continue
		}
		if err := validate(child(path, name), properties[name], value); err != nil {
			return err
		}
	}
	additional, ok := schema["additionalProperties"]
	if !ok {
		return nil
	}
	for _, name := range sortedKeys(object) {
		if _, declared := properties[name]; declared {
			continue
		}
		if allowed, ok := additional.(bool); ok && !allowed {
			return &SchemaError{Path: child(path, name), Reason: "is not a property the schema declares"}
		}
		if err := validate(child(path, name), additional, object[name]); err != nil {
			return err
		}
	}
	return nil
}

func checkArray(path string, schema map[string]any, document any) error {
	array, ok := document.([]any)
	if !ok {
		return nil
	}
	if limit, ok := schema["minItems"]; ok {
		if n, err := count(path, "minItems", limit); err != nil {
			return err
		} else if len(array) < n {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("has fewer than %d items", n)}
		}
	}
	if limit, ok := schema["maxItems"]; ok {
		if n, err := count(path, "maxItems", limit); err != nil {
			return err
		} else if len(array) > n {
			return &SchemaError{Path: path, Reason: fmt.Sprintf("has more than %d items", n)}
		}
	}
	items, ok := schema["items"]
	if !ok {
		return nil
	}
	for i, item := range array {
		if err := validate(child(path, strconv.Itoa(i)), items, item); err != nil {
			return err
		}
	}
	return nil
}

func count(path, keyword string, value any) (int, error) {
	number, ok := normalize(value).(float64)
	if !ok || number < 0 || number != float64(int64(number)) {
		return 0, fmt.Errorf("schema: %s: %s is a non-negative integer", pathOrRoot(path), keyword)
	}
	return int(number), nil
}

// equal compares two normalized JSON values.
func equal(want, got any) bool {
	switch typed := want.(type) {
	case map[string]any:
		other, ok := got.(map[string]any)
		if !ok || len(other) != len(typed) {
			return false
		}
		for key, value := range typed {
			item, present := other[key]
			if !present || !equal(value, item) {
				return false
			}
		}
		return true
	case []any:
		other, ok := got.([]any)
		if !ok || len(other) != len(typed) {
			return false
		}
		for i, value := range typed {
			if !equal(value, other[i]) {
				return false
			}
		}
		return true
	default:
		return want == got
	}
}

// render is a schema value as an error should show it.
func render(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// child is the JSON Pointer of a member, escaped as RFC 6901 requires.
func child(path, name string) string {
	escaped := strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
	return path + "/" + escaped
}

func pathOrRoot(path string) string {
	if path == "" {
		return "the document"
	}
	return path
}

func sortedKeys(object map[string]any) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
