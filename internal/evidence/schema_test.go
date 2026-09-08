package evidence_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/evidence"
)

var _ = Describe("JSON Schema validation", Label("unit"), func() {
	const schema = `{
      "type": "object",
      "required": ["decision", "findings"],
      "properties": {
        "decision": {"type": "string", "enum": ["approve", "revise"]},
        "attempts": {"type": "integer", "minimum": 1, "maximum": 3},
        "findings": {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "object",
            "required": ["summary"],
            "properties": {
              "summary": {"type": "string", "minLength": 1},
              "location": {"type": "string", "pattern": "^[^:]+:[0-9]+$"}
            },
            "additionalProperties": false
          }
        }
      },
      "additionalProperties": false
    }`

	valid := map[string]any{
		"decision": "approve",
		"attempts": 2,
		"findings": []any{map[string]any{"summary": "a note", "location": "internal/evidence/evidence.go:12"}},
	}

	It("accepts a document that satisfies every constraint", func() {
		Expect(evidence.ValidateJSON(schema, valid)).To(Succeed())
	})

	DescribeTable("names the violating path",
		func(mutate func(map[string]any), path, reason string) {
			document := map[string]any{
				"decision": "approve",
				"attempts": 2,
				"findings": []any{map[string]any{"summary": "a note", "location": "a.go:1"}},
			}
			mutate(document)

			err := evidence.ValidateJSON(schema, document)

			var invalid *evidence.SchemaError
			Expect(errors.As(err, &invalid)).To(BeTrue(), "%v", err)
			Expect(invalid.Path).To(Equal(path))
			Expect(invalid.Reason).To(ContainSubstring(reason))
		},
		Entry("a value outside an enum", func(d map[string]any) { d["decision"] = "needs-human" },
			"/decision", `is not one of "approve", "revise"`),
		Entry("a value of the wrong type", func(d map[string]any) { d["attempts"] = "two" },
			"/attempts", "is a string, not integer"),
		Entry("a number below its minimum", func(d map[string]any) { d["attempts"] = 0 },
			"/attempts", "is below 1"),
		Entry("a missing required property", func(d map[string]any) { delete(d, "findings") },
			"/findings", "is required and missing"),
		Entry("an empty array where the schema wants one item", func(d map[string]any) { d["findings"] = []any{} },
			"/findings", "has fewer than 1 items"),
		Entry("a violation inside an array element", func(d map[string]any) {
			d["findings"] = []any{
				map[string]any{"summary": "fine", "location": "a.go:1"},
				map[string]any{"summary": "", "location": "a.go:2"},
			}
		}, "/findings/1/summary", "is shorter than 1 characters"),
		Entry("a string that does not match its pattern", func(d map[string]any) {
			d["findings"] = []any{map[string]any{"summary": "s", "location": "nowhere"}}
		}, "/findings/0/location", "does not match"),
		Entry("a property the schema does not declare", func(d map[string]any) { d["mood"] = "confident" },
			"/mood", "is not a property the schema declares"),
	)

	It("names the same first violation whichever order the document is built in", func() {
		document := map[string]any{
			"decision": "sideways",
			"attempts": "many",
			"findings": []any{},
		}

		for range 8 {
			err := evidence.ValidateJSON(schema, document)
			Expect(err).To(MatchError(ContainSubstring("/attempts is a string, not integer")))
		}
	})

	It("refuses a schema whose keywords it does not implement, rather than ignoring them", func() {
		err := evidence.ValidateJSON(`{"type": "object", "properties": {"a": {"oneOf": [{"type": "string"}]}}}`,
			map[string]any{"a": 1})

		var unsupported *evidence.UnsupportedKeywordError
		Expect(errors.As(err, &unsupported)).To(BeTrue(), "%v", err)
		Expect(unsupported.Keyword).To(Equal("oneOf"))
		Expect(unsupported.Path).To(Equal("/a"))
	})

	It("refuses a document validated against no schema at all", func() {
		Expect(evidence.ValidateJSON("  ", map[string]any{})).
			To(MatchError(ContainSubstring("validated against no schema")))
	})
})
