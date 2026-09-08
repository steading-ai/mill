package contract_test

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// entityMessages maps a design document's entity tables onto the messages
// that carry them. docs/domain-model.md is the authority for every field;
// docs/ledger.md's tables add the columns the store needs and the model does
// not name.
//
// Two of the ledger's tables are deliberately absent. Its `runs` table omits
// `generation` and calls `node_id` unique outright, which contradicts
// docs/domain-model.md §Restart; its `workflows` table stores the steps as a
// single `definition` document, which the proto types out instead. Both are
// covered by the domain model's own Run and Workflow tables, and Run's proto
// comment records the disagreement.
var (
	domainMessages = map[string]string{
		"Run":             "Run",
		"Workflow":        "Workflow",
		"Project default": "ProjectDefault",
		"Step":            "Step",
		"Session":         "Session",
		"Evidence":        "Evidence",
		"Check":           "Check",
		"Gate":            "Gate",
		"Workspace":       "Workspace",
		"Step token":      "StepToken",
	}
	ledgerMessages = map[string]string{
		"`step_runs`":   "StepRun",
		"`evidence`":    "Evidence",
		"`sessions`":    "Session",
		"`step_tokens`": "StepToken",
		"`usage`":       "Usage",
		"`workspaces`":  "Workspace",
		"`skills`":      "Skill",
	}
)

var _ = Describe("mill.proto", func() {
	var c contract

	BeforeEach(func() {
		c = parse(millProto).merge(parse(storeProto))
		Expect(c.messages).NotTo(BeEmpty())
		Expect(c.fields).NotTo(BeEmpty())
		Expect(c.enums).NotTo(BeEmpty())
	})

	It("gives every entity table in the domain model a message whose fields match it", func() {
		tables := parseTables(domainModel)
		Expect(tables).NotTo(BeEmpty())

		for heading, message := range domainMessages {
			table := tableFor(tables, heading)
			Expect(table.fields).NotTo(BeEmpty(), "table under %s", heading)
			Expect(c.messages).To(HaveKey(message), "domain model §%s", heading)

			for _, name := range table.fields {
				f, ok := c.lookup(message, name)
				Expect(ok).To(BeTrue(), "%s has no field %s, named by domain model §%s",
					message, name, heading)
				Expect(f.comment).NotTo(BeEmpty(), "%s.%s carries no note", message, name)
			}
		}
	})

	It("gives every ledger table a message whose fields match it", func() {
		tables := parseTables(ledgerDoc)
		Expect(tables).NotTo(BeEmpty())

		for heading, message := range ledgerMessages {
			table := tableFor(tables, heading)
			Expect(table.fields).NotTo(BeEmpty(), "table under %s", heading)

			for _, name := range table.fields {
				f, ok := c.lookup(message, name)
				Expect(ok).To(BeTrue(), "%s has no field %s, named by ledger §%s",
					message, name, heading)
				Expect(f.comment).NotTo(BeEmpty(), "%s.%s carries no note", message, name)
			}
		}
	})

	It("comments every message and field", func() {
		for name, comment := range c.messages {
			Expect(comment).NotTo(BeEmpty(), "message %s", name)
		}
		for _, f := range c.fields {
			Expect(f.comment).NotTo(BeEmpty(), "%s.%s", f.message, f.name)
		}
	})

	It("gives every enum an _UNSPECIFIED zero value", func() {
		for name, values := range c.enums {
			Expect(values).NotTo(BeEmpty(), "enum %s", name)
			Expect(values[0].number).To(Equal("0"), "enum %s", name)
			Expect(values[0].name).To(HaveSuffix("_UNSPECIFIED"), "enum %s", name)
		}
	})

	It("defines every message and enum the work order names", func() {
		for _, m := range []string{"Run", "StepRun", "Evidence", "Session", "Workflow",
			"Step", "Check", "Gate", "StepToken", "Usage", "Workspace"} {
			Expect(c.messages).To(HaveKey(m))
		}
		for _, e := range []string{"RunStatus", "StepRunStatus", "EvidenceKind", "Capability"} {
			Expect(c.enums).To(HaveKey(e))
		}
	})

	It("types Evidence as a oneof over its kinds", func() {
		Expect(c.field("Evidence", "value").typ).To(Equal("oneof"))

		// Every kind the domain model names has a member of the union, so a
		// row cannot claim a kind the wire cannot carry.
		for _, kind := range []string{"commit", "exit", "verdict", "structured",
			"url", "status", "artifact", "gate"} {
			_, ok := c.lookup("Evidence", kind)
			Expect(ok).To(BeTrue(), "Evidence has no %s member", kind)
		}
		for _, v := range c.enums["EvidenceKind"][1:] {
			member := strings.ToLower(strings.TrimPrefix(v.name, "EVIDENCE_KIND_"))
			_, ok := c.lookup("Evidence", member)
			Expect(ok).To(BeTrue(), "EvidenceKind names %s with no member on Evidence", v.name)
		}
	})

	It("records that harness settings live on the step", func() {
		// The question docs/domain-model.md left open is settled in the doc
		// and in the proto, which is where an implementer meets it.
		Expect(c.messages["HarnessSettings"]).To(ContainSubstring("on the step"))
		Expect(c.field("Step", "harness").typ).To(Equal("map<string, HarnessSettings>"))

		doc, err := os.ReadFile(domainModel)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(doc)).To(ContainSubstring(
			"Harness settings live on the **step**, not on the skill record."))

		_, open, _ := strings.Cut(string(doc), "## Open questions")
		Expect(open).NotTo(ContainSubstring("harness"),
			"the settled question is still listed as open")
	})
})
