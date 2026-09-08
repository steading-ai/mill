package run

import (
	"fmt"
	"strconv"
	"strings"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// A gate's `auto` predicate, and the little language it is written in.
//
// docs/domain-model.md §Gate defines it as "a predicate over prior evidence
// and the run's tier, e.g. `verdict.decision == approve && tier <= feature`".
// That sentence is the whole specification, so the language here is exactly
// what it describes and nothing else: comparisons of a name against a
// literal, joined by `&&` and `||`, negated by `!`, grouped by parentheses.
// There are no arithmetic operators, no function calls, and no way to reach
// anything the run has not already established as evidence.
//
// Three rules make it safe to be wrong in:
//
//   - A bare word that names nothing in the environment is a string literal,
//     so `approve` and `feature` need no quotes.
//   - A dotted path that resolves to nothing is undefined, and undefined
//     compares equal to nothing at all. A gate whose evidence has not been
//     produced is therefore not auto-approved.
//   - `tier` compares by rank, not by spelling, so `tier <= feature` means
//     what it reads as.

// predicateEnv is what a predicate may see: the run's tier, and the evidence
// prior steps closed on, under both the step's name and its evidence kind.
type predicateEnv map[string]any

// tierValue is a tier as an operand: comparable by rank against another
// tier's name.
type tierValue millv1.Tier

// undefined is a path that resolved to nothing. It equals nothing, including
// itself, so a predicate over evidence that does not exist is false rather
// than accidentally true.
type undefined struct{}

// newPredicateEnv builds the environment from the run and its ledger.
func newPredicateEnv(run *millv1.Run, detail *millv1.InspectResponse) predicateEnv {
	env := predicateEnv{"tier": tierValue(run.GetTier())}
	for _, d := range detail.GetSteps() {
		value := evidenceValue(d.GetEvidence())
		if value == nil {
			continue
		}
		// Under the step's name, and under the kind of fact it is. The
		// ledger is in order, so a later attempt's evidence replaces an
		// earlier one's.
		env[d.GetStepRun().GetStepName()] = value
		env[evidenceName(d.GetEvidence().GetKind())] = value
	}
	return env
}

// evidenceValue is one piece of evidence as a predicate sees it: a map of
// fields, with `value` holding the scalar for the kinds that are one.
func evidenceValue(e *millv1.Evidence) map[string]any {
	switch e.GetKind() {
	case millv1.EvidenceKind_EVIDENCE_KIND_VERDICT:
		blocking := 0
		for _, finding := range e.GetVerdict().GetFindings() {
			if finding.GetSeverity() == millv1.FindingSeverity_FINDING_SEVERITY_BLOCKING {
				blocking++
			}
		}
		return map[string]any{
			"decision": strings.ToLower(strings.TrimPrefix(e.GetVerdict().GetDecision().String(), "VERDICT_DECISION_")),
			"findings": float64(len(e.GetVerdict().GetFindings())),
			"blocking": float64(blocking),
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_EXIT:
		return map[string]any{"status": float64(e.GetExit().GetStatus())}
	case millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED:
		return e.GetStructured().AsMap()
	case millv1.EvidenceKind_EVIDENCE_KIND_COMMIT:
		return map[string]any{"value": e.GetCommit()}
	case millv1.EvidenceKind_EVIDENCE_KIND_URL:
		return map[string]any{"value": e.GetUrl()}
	case millv1.EvidenceKind_EVIDENCE_KIND_STATUS:
		return map[string]any{"value": e.GetStatus()}
	case millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT:
		return map[string]any{"value": e.GetArtifact()}
	case millv1.EvidenceKind_EVIDENCE_KIND_GATE:
		return map[string]any{"value": e.GetGate()}
	default:
		return nil
	}
}

// evaluate reports whether a predicate holds in an environment. An empty
// predicate never holds: a gate with no policy is a gate for a human.
func evaluate(predicate string, env predicateEnv) (bool, error) {
	text := strings.TrimSpace(predicate)
	if text == "" {
		return false, nil
	}
	tokens, err := lex(text)
	if err != nil {
		return false, err
	}
	p := &parser{tokens: tokens, env: env}
	value, err := p.expression()
	if err != nil {
		return false, err
	}
	if p.next != len(p.tokens) {
		return false, fmt.Errorf("gate predicate %q: unexpected %q", predicate, p.tokens[p.next])
	}
	return value, nil
}

// lex splits a predicate into words, operators and grouping. Quoted strings
// keep their quotes so the parser can tell one from a bare word.
func lex(text string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case strings.HasPrefix(text[i:], "&&"), strings.HasPrefix(text[i:], "||"),
			strings.HasPrefix(text[i:], "=="), strings.HasPrefix(text[i:], "!="),
			strings.HasPrefix(text[i:], "<="), strings.HasPrefix(text[i:], ">="):
			tokens = append(tokens, text[i:i+2])
			i += 2
		case c == '(' || c == ')' || c == '!' || c == '<' || c == '>':
			tokens = append(tokens, text[i:i+1])
			i++
		case c == '\'' || c == '"':
			end := strings.IndexByte(text[i+1:], c)
			if end < 0 {
				return nil, fmt.Errorf("gate predicate: unterminated string in %q", text)
			}
			tokens = append(tokens, text[i:i+end+2])
			i += end + 2
		default:
			j := i
			for j < len(text) && isWordByte(text[j]) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("gate predicate: %q is not an operator", text[i:i+1])
			}
			tokens = append(tokens, text[i:j])
			i = j
		}
	}
	return tokens, nil
}

// isWordByte reports whether a byte may appear in a name or a number.
func isWordByte(c byte) bool {
	return c == '.' || c == '_' || c == '-' || c == ':' || c == '/' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// parser is a recursive descent over the token list.
type parser struct {
	tokens []string
	next   int
	env    predicateEnv
}

// peek is the next token, or empty at the end.
func (p *parser) peek() string {
	if p.next >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.next]
}

// take consumes a token when it is the one expected.
func (p *parser) take(token string) bool {
	if p.peek() != token {
		return false
	}
	p.next++
	return true
}

// expression is a disjunction, the loosest binding.
func (p *parser) expression() (bool, error) {
	value, err := p.conjunction()
	if err != nil {
		return false, err
	}
	for p.take("||") {
		right, err := p.conjunction()
		if err != nil {
			return false, err
		}
		value = value || right
	}
	return value, nil
}

// conjunction binds tighter than `||`.
func (p *parser) conjunction() (bool, error) {
	value, err := p.unary()
	if err != nil {
		return false, err
	}
	for p.take("&&") {
		right, err := p.unary()
		if err != nil {
			return false, err
		}
		value = value && right
	}
	return value, nil
}

// unary is a negation or a primary.
func (p *parser) unary() (bool, error) {
	if p.take("!") {
		value, err := p.unary()
		return !value, err
	}
	return p.primary()
}

// primary is a parenthesised expression or a comparison.
func (p *parser) primary() (bool, error) {
	if p.take("(") {
		value, err := p.expression()
		if err != nil {
			return false, err
		}
		if !p.take(")") {
			return false, fmt.Errorf("gate predicate: expected ) at %q", p.peek())
		}
		return value, nil
	}
	return p.comparison()
}

// comparison is one operand, optionally compared against another. A bare
// operand is true only when it is the literal `true`.
func (p *parser) comparison() (bool, error) {
	left, err := p.operand()
	if err != nil {
		return false, err
	}
	switch op := p.peek(); op {
	case "==", "!=", "<", "<=", ">", ">=":
		p.next++
		right, err := p.operand()
		if err != nil {
			return false, err
		}
		return compare(left, op, right)
	default:
		truth, ok := left.(bool)
		if !ok {
			return false, fmt.Errorf("gate predicate: %v is not a condition", left)
		}
		return truth, nil
	}
}

// operand resolves one name or literal.
func (p *parser) operand() (any, error) {
	token := p.peek()
	if token == "" {
		return nil, fmt.Errorf("gate predicate: an operand is missing")
	}
	p.next++
	switch {
	case token == "true":
		return true, nil
	case token == "false":
		return false, nil
	case len(token) > 1 && (token[0] == '\'' || token[0] == '"'):
		return token[1 : len(token)-1], nil
	}
	if number, err := strconv.ParseFloat(token, 64); err == nil {
		return number, nil
	}
	if !isName(token) {
		return nil, fmt.Errorf("gate predicate: %q is not a name", token)
	}
	return p.lookup(token), nil
}

// isName reports whether a token could name something rather than being an
// operator.
func isName(token string) bool {
	for i := 0; i < len(token); i++ {
		if !isWordByte(token[i]) {
			return false
		}
	}
	return token != ""
}

// lookup resolves a name in the environment. A bare word that names nothing
// is a string literal; a dotted path that resolves to nothing is undefined.
func (p *parser) lookup(name string) any {
	head, rest, dotted := strings.Cut(name, ".")
	value, ok := p.env[head]
	if !ok {
		if dotted {
			return undefined{}
		}
		return name
	}
	if !dotted {
		if fields, isMap := value.(map[string]any); isMap {
			if scalar, has := fields["value"]; has {
				return scalar
			}
			return undefined{}
		}
		return value
	}
	fields, isMap := value.(map[string]any)
	if !isMap {
		return undefined{}
	}
	for _, part := range strings.Split(rest, ".") {
		field, has := fields[part]
		if !has {
			return undefined{}
		}
		if next, isMap := field.(map[string]any); isMap {
			fields = next
			continue
		}
		return field
	}
	return fields
}

// compare applies one operator to two operands. Tiers compare by rank,
// numbers numerically, strings by equality only, and undefined equals
// nothing.
func compare(left any, op string, right any) (bool, error) {
	if _, ok := left.(undefined); ok {
		return op == "!=", nil
	}
	if _, ok := right.(undefined); ok {
		return op == "!=", nil
	}
	if tier, ok := left.(tierValue); ok {
		return compareTier(tier, op, right)
	}
	if tier, ok := right.(tierValue); ok {
		flipped, err := compareTier(tier, flip(op), left)
		return flipped, err
	}
	if a, ok := left.(float64); ok {
		b, ok := right.(float64)
		if !ok {
			return false, fmt.Errorf("gate predicate: %v is not a number", right)
		}
		return ordered(a, b, op)
	}
	a, b := fmt.Sprint(left), fmt.Sprint(right)
	switch op {
	case "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	default:
		return false, fmt.Errorf("gate predicate: %q does not order strings", op)
	}
}

// compareTier compares a tier against a tier's name by rank.
func compareTier(tier tierValue, op string, other any) (bool, error) {
	name := fmt.Sprint(other)
	rank, ok := tierRanks[name]
	if !ok {
		return false, fmt.Errorf("gate predicate: %q is not a tier", name)
	}
	return ordered(float64(tier), float64(rank), op)
}

// flip turns an operator around, for a comparison written the other way up.
func flip(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default:
		return op
	}
}

// ordered applies a comparison to two ranks.
func ordered(a, b float64, op string) (bool, error) {
	switch op {
	case "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	case "<":
		return a < b, nil
	case "<=":
		return a <= b, nil
	case ">":
		return a > b, nil
	case ">=":
		return a >= b, nil
	default:
		return false, fmt.Errorf("gate predicate: %q is not an operator", op)
	}
}

// tierRanks is the tier vocabulary in the order docs/domain-model.md lists
// it, which is what makes `tier <= feature` mean what it reads as.
var tierRanks = map[string]millv1.Tier{
	"quick-fix": millv1.Tier_TIER_QUICK_FIX,
	"quick_fix": millv1.Tier_TIER_QUICK_FIX,
	"small":     millv1.Tier_TIER_SMALL,
	"feature":   millv1.Tier_TIER_FEATURE,
	"large":     millv1.Tier_TIER_LARGE,
}
