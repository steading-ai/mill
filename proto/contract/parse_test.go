// Package contract_test holds the specs over the proto contracts themselves:
// what mill.v1 and harness.v1 must declare, and how they must relate to the
// design documents that are their input. buf owns syntax and comment
// presence; these specs own meaning.
package contract_test

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Paths are relative to this package's directory, which is where Ginkgo runs
// its specs from.
const (
	millProto    = "../mill/v1/mill.proto"
	storeProto   = "../mill/v1/store.proto"
	harnessProto = "../harness/v1/harness.proto"
	domainModel  = "../../docs/domain-model.md"
	ledgerDoc    = "../../docs/ledger.md"
)

// field is one field declaration and the comment above it. A oneof is
// recorded as a field of type "oneof", because the domain model's tables name
// it the same way its members' union is named.
type field struct {
	message  string
	name     string
	typ      string
	repeated bool
	comment  string
}

// rpc is one method of a service.
type rpc struct {
	service  string
	name     string
	request  string
	response string
	comment  string
}

// enumValue is one enumerator and its number.
type enumValue struct {
	name   string
	number string
}

// contract is a proto file reduced to what the specs assert over.
type contract struct {
	services map[string]string // service name to its leading comment
	messages map[string]string // message name to its leading comment
	fields   []field
	enums    map[string][]enumValue
	rpcs     []rpc
}

// field returns the named field of a message, failing the spec if the
// message does not declare it.
func (c contract) field(message, name string) field {
	f, ok := c.lookup(message, name)
	if !ok {
		Fail(message + " has no field " + name)
	}
	return f
}

// lookup returns the named field of a message and whether it exists.
func (c contract) lookup(message, name string) (field, bool) {
	for _, f := range c.fields {
		if f.message == message && f.name == name {
			return f, true
		}
	}
	return field{}, false
}

// rpcNames returns every method name, in declaration order.
func (c contract) rpcNames() []string {
	out := make([]string, 0, len(c.rpcs))
	for _, r := range c.rpcs {
		out = append(out, r.name)
	}
	return out
}

// merge folds another parsed file into this one, so specs can assert across
// the two files of one proto package.
func (c contract) merge(other contract) contract {
	for k, v := range other.services {
		c.services[k] = v
	}
	for k, v := range other.messages {
		c.messages[k] = v
	}
	for k, v := range other.enums {
		c.enums[k] = v
	}
	c.fields = append(c.fields, other.fields...)
	c.rpcs = append(c.rpcs, other.rpcs...)
	return c
}

// Match against a single trimmed line of the proto.
var (
	messageRe   = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
	enumRe      = regexp.MustCompile(`^enum\s+(\w+)\s*\{`)
	oneofRe     = regexp.MustCompile(`^oneof\s+(\w+)\s*\{`)
	serviceRe   = regexp.MustCompile(`^service\s+(\w+)\s*\{`)
	rpcRe       = regexp.MustCompile(`^rpc\s+(\w+)\s*\(\s*([\w.]+)\s*\)\s+returns\s+\(\s*(stream\s+)?([\w.]+)\s*\)\s*;`)
	fieldRe     = regexp.MustCompile(`^(repeated\s+|optional\s+)?(map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+\s*;`)
	enumValueRe = regexp.MustCompile(`^(\w+)\s*=\s*(\d+)\s*;`)
)

// parse reads a proto file with a scanner that understands exactly the
// grammar these files use: line comments, messages, enums, oneofs, services,
// and one declaration per line. It does not need to be a full parser; buf
// owns syntax, and this only pairs declarations with their comments.
func parse(path string) contract {
	src, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())

	c := contract{
		services: map[string]string{},
		messages: map[string]string{},
		enums:    map[string][]enumValue{},
	}
	var (
		comment []string
		scope   []string // enclosing declarations, innermost last
		message string
		enum    string
		service string
	)
	take := func() string {
		s := strings.Join(comment, " ")
		comment = nil
		return s
	}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "//"):
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(line, "//")))
		case line == "":
			comment = nil
		case messageRe.MatchString(line):
			message = messageRe.FindStringSubmatch(line)[1]
			c.messages[message] = take()
			scope = append(scope, "message")
		case enumRe.MatchString(line):
			enum = enumRe.FindStringSubmatch(line)[1]
			c.enums[enum] = nil
			take()
			scope = append(scope, "enum")
		case message != "" && oneofRe.MatchString(line):
			c.fields = append(c.fields, field{
				message: message, name: oneofRe.FindStringSubmatch(line)[1],
				typ: "oneof", comment: take(),
			})
			scope = append(scope, "oneof")
		case serviceRe.MatchString(line):
			service = serviceRe.FindStringSubmatch(line)[1]
			c.services[service] = take()
			scope = append(scope, "service")
		case line == "}":
			Expect(scope).NotTo(BeEmpty(), "unbalanced brace in "+path)
			switch scope[len(scope)-1] {
			case "message":
				message = ""
			case "enum":
				enum = ""
			case "service":
				service = ""
			}
			scope = scope[:len(scope)-1]
		case service != "" && rpcRe.MatchString(line):
			m := rpcRe.FindStringSubmatch(line)
			c.rpcs = append(c.rpcs, rpc{
				service: service, name: m[1], request: m[2],
				response: m[4], comment: take(),
			})
		case enum != "" && enumValueRe.MatchString(line):
			m := enumValueRe.FindStringSubmatch(line)
			take()
			c.enums[enum] = append(c.enums[enum], enumValue{name: m[1], number: m[2]})
		case message != "" && fieldRe.MatchString(line):
			m := fieldRe.FindStringSubmatch(line)
			c.fields = append(c.fields, field{
				message: message, name: m[3], typ: m[2],
				repeated: strings.TrimSpace(m[1]) == "repeated", comment: take(),
			})
		default:
			comment = nil
		}
	}
	Expect(sc.Err()).NotTo(HaveOccurred())
	Expect(scope).To(BeEmpty(), "unbalanced brace in "+path)
	return c
}

// docTable is one entity table of a design document: the heading it sits
// under and the identifiers its first column names.
type docTable struct {
	heading string
	fields  []string
}

var (
	headingRe = regexp.MustCompile(`^#{2,4}\s+(.*)$`)
	identRe   = regexp.MustCompile("`([a-z_][a-z0-9_]*)`")
)

// parseTables reads a markdown document and returns its entity tables: the
// ones whose header row is Field or Column against Notes. Every other table
// in these documents describes something that is not a record — what a
// capability grants, what an evidence kind proves — and is skipped.
func parseTables(path string) []docTable {
	src, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())

	var (
		out     []docTable
		heading string
		current *docTable
	)
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case headingRe.MatchString(line):
			current = nil
			heading = strings.TrimSpace(headingRe.FindStringSubmatch(line)[1])
		case strings.HasPrefix(line, "|"):
			cells := strings.Split(strings.Trim(line, "|"), "|")
			first := strings.TrimSpace(cells[0])
			switch {
			case strings.EqualFold(first, "Field"), strings.EqualFold(first, "Column"):
				if len(cells) > 1 && strings.EqualFold(strings.TrimSpace(cells[1]), "Notes") {
					out = append(out, docTable{heading: heading})
					current = &out[len(out)-1]
				}
			case current != nil:
				for _, m := range identRe.FindAllStringSubmatch(first, -1) {
					current.fields = append(current.fields, m[1])
				}
			}
		default:
			current = nil
		}
	}
	Expect(sc.Err()).NotTo(HaveOccurred())
	return out
}

// tableFor returns the table under a heading, failing the spec if the
// document has none.
func tableFor(tables []docTable, heading string) docTable {
	GinkgoHelper()
	for _, t := range tables {
		if t.heading == heading {
			return t
		}
	}
	Fail("no entity table under heading " + heading)
	return docTable{}
}
