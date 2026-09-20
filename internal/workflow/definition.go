package workflow

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
)

// document is `workflow.yaml`: the mill record for one workflow, mapping
// field for field onto mill.v1.Workflow. It is decoded with unknown fields
// refused, so a misspelled key is an error at push rather than a setting
// silently dropped from a run.
type document struct {
	Name    string      `yaml:"name"`
	Scope   string      `yaml:"scope,omitempty"`
	Project string      `yaml:"project,omitempty"`
	Status  *statusYAML `yaml:"status,omitempty"`
	Steps   []stepYAML  `yaml:"steps"`
}

// statusYAML is how a `status` step watches the forge's checks. It is
// workflow-level because every CI step in one workflow watches the same
// forge at the same cadence, and the numbers are a property of the project's
// pipeline rather than of any one step.
type statusYAML struct {
	Interval string `yaml:"interval,omitempty"`
	Timeout  string `yaml:"timeout,omitempty"`
}

// stepYAML is one step. `kind` decides which of skill, check, and gate is
// meaningful; the others must be absent.
type stepYAML struct {
	Name         string                    `yaml:"name"`
	Kind         string                    `yaml:"kind"`
	Session      string                    `yaml:"session,omitempty"`
	Skill        string                    `yaml:"skill,omitempty"`
	Check        *checkYAML                `yaml:"check,omitempty"`
	Gate         *gateYAML                 `yaml:"gate,omitempty"`
	Artifact     *artifactYAML             `yaml:"artifact,omitempty"`
	Evidence     string                    `yaml:"evidence"`
	Capabilities []string                  `yaml:"capabilities,omitempty"`
	Harness      map[string]map[string]any `yaml:"harness,omitempty"`
	OnFailure    string                    `yaml:"on_failure,omitempty"`
	Retries      int32                     `yaml:"retries,omitempty"`
	Timeout      string                    `yaml:"timeout,omitempty"`
	OutputSchema string                    `yaml:"output_schema,omitempty"`
	TokenBudget  int64                     `yaml:"token_budget,omitempty"`
	Models       []string                  `yaml:"models,omitempty"`
}

// artifactYAML says where an `artifact` step's evidence is read from. A step
// that leaves it out closes on the session's final message.
type artifactYAML struct {
	Path string `yaml:"path"`
}

type checkYAML struct {
	Command      []string `yaml:"command"`
	Exit         []int32  `yaml:"exit,omitempty"`
	Stdout       string   `yaml:"stdout,omitempty"`
	StdoutSchema string   `yaml:"stdout_schema,omitempty"`
	Feedback     string   `yaml:"feedback,omitempty"`
}

type gateYAML struct {
	Ask        askYAML   `yaml:"ask"`
	Auto       predicate `yaml:"auto,omitempty"`
	DenyResult string    `yaml:"deny_result,omitempty"`
}

type askYAML struct {
	Title string `yaml:"title"`
	Body  string `yaml:"body,omitempty"`
}

const (
	// harnessName is the key a step's `harness` table uses for the Claude
	// Agent SDK adapter, matching the package that implements it
	// (internal/harness/claude).
	harnessName = "claude"

	// retiredHarnessName is a spelling of that key no adapter answers to.
	// It is refused rather than aliased: a definition that uses it carries
	// settings nothing would read, and a silently dropped model or tool
	// policy is the failure mode this document's strict decoding exists to
	// prevent.
	retiredHarnessName = "claude-code"
)

// A step's `harness` table maps a harness name to that harness's options
// document, and the loader reads nothing inside one. What a knob means is
// the harness's to say: the block is carried to the adapter verbatim and the
// harness validates it, so a knob a harness grows costs a workflow change
// and nothing else (docs/domain-model.md §Step).
//
// The one thing the loader insists on is that the block is a mapping, which
// is what a document has to be to survive the wire as a
// google.protobuf.Struct. That is `map[string]any` and not a named type on
// purpose: yaml.v3 decodes a nested mapping into the map type that contains
// it, and a named one would reach structpb as something it refuses.

// predicate is a gate's auto-approve policy: an expression over prior
// evidence and the run's tier. `auto: true` is the policy that always
// holds, which is how a workflow says "merge without asking" —
// docs/core-principles.md §1, repo protections are the repo's job — and it
// is written as a bool because that is what it reads as.
type predicate string

// alwaysAuto is what `auto: true` means once stored: a predicate that holds
// for every run.
const alwaysAuto = "true"

func (p *predicate) UnmarshalYAML(node *yaml.Node) error {
	var b bool
	if node.Tag == "!!bool" {
		if err := node.Decode(&b); err != nil {
			return err
		}
		if !b {
			// `auto: false` is "always ask", which is what leaving the
			// policy out already means. Two spellings for one thing invites
			// a definition that says false and means never.
			return fmt.Errorf("line %d: auto: false is not a policy; leave auto out to always ask", node.Line)
		}
		*p = alwaysAuto
		return nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	*p = predicate(s)
	return nil
}

// decode reads workflow.yaml. Unknown fields are refused: a key the loader
// does not understand is a setting the run would not honour.
func decode(content []byte) (document, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(content)))
	dec.KnownFields(true)
	var doc document
	if err := dec.Decode(&doc); err != nil {
		return document{}, fmt.Errorf("%s: %w", DefinitionFile, err)
	}
	return doc, nil
}

// encode writes workflow.yaml back. `mill workflow pull` uses it, so the
// two directions have to agree: a pulled definition re-loads to the same
// version it was pulled at.
func encode(doc document) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode %s: %w", DefinitionFile, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode %s: %w", DefinitionFile, err)
	}
	return []byte(b.String()), nil
}

// defaultRetries is how many times a skill step may fail with revise before
// the circuit breaker parks the run. docs/domain-model.md §Step.
//
// A check gets no such default: runCheck takes no feedback from one attempt
// to the next, so retrying it changes nothing unless the world does, and a
// check that wants a retry anyway says so with an explicit `retries`.
const defaultRetries = 3

// toProto turns the decoded document into the definition the store holds.
// It fills the documented defaults — org scope, park on failure, three
// retries — so that what is stored is what a run reads, with nothing left
// to interpret at execution time.
func (d document) toProto() (*millv1.Workflow, error) {
	scope := millv1.Scope_SCOPE_ORG
	if d.Scope != "" {
		var err error
		if scope, err = parseEnum[millv1.Scope](d.Scope, "scope"); err != nil {
			return nil, err
		}
	}
	w := &millv1.Workflow{Name: d.Name, Scope: scope, Project: d.Project}
	status, err := d.Status.toProto()
	if err != nil {
		return nil, err
	}
	w.Status = status
	for _, s := range d.Steps {
		step, err := s.toProto()
		if err != nil {
			return nil, err
		}
		w.Steps = append(w.Steps, step)
	}
	return w, nil
}

// toProto fills the documented defaults for a `status` step's watch — the
// interval it asks on and the cap it gives up at — so that what is stored is
// what a run reads. [github.com/ivy/mill/internal/evidence] owns the
// numbers; the loader only writes them down.
func (s *statusYAML) toProto() (*millv1.StatusPolicy, error) {
	policy := &millv1.StatusPolicy{
		Interval: durationpb.New(evidence.DefaultStatusInterval),
		Timeout:  durationpb.New(evidence.DefaultStatusTimeout),
	}
	if s == nil {
		return policy, nil
	}
	for _, field := range []struct {
		name  string
		text  string
		field **durationpb.Duration
	}{
		{"interval", s.Interval, &policy.Interval},
		{"timeout", s.Timeout, &policy.Timeout},
	} {
		if field.text == "" {
			continue
		}
		d, err := time.ParseDuration(field.text)
		if err != nil {
			return nil, fmt.Errorf("status: %s: %w", field.name, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("status: %s: %s is not a positive duration", field.name, field.text)
		}
		*field.field = durationpb.New(d)
	}
	return policy, nil
}

func (s stepYAML) toProto() (*millv1.Step, error) {
	fail := func(err error) (*millv1.Step, error) {
		return nil, fmt.Errorf("step %s: %w", s.Name, err)
	}
	kind, err := parseEnum[millv1.StepKind](s.Kind, "kind")
	if err != nil {
		return fail(err)
	}
	evidence, err := parseEnum[millv1.EvidenceKind](s.Evidence, "evidence")
	if err != nil {
		return fail(err)
	}
	step := &millv1.Step{
		Name:         s.Name,
		Kind:         kind,
		Session:      s.Session,
		Evidence:     evidence,
		Retries:      s.Retries,
		OutputSchema: s.OutputSchema,
		TokenBudget:  s.TokenBudget,
		Models:       s.Models,
	}
	// A preparatory step is never failed, so it has no retries to spend
	// and no on_failure to consult (docs/sandbox.md §"Preparatory steps");
	// a definition that writes either has misread the kind, and is told.
	if kind == millv1.StepKind_STEP_KIND_PREPARATORY {
		if s.Retries != 0 || s.OnFailure != "" {
			return fail(fmt.Errorf("a preparatory step never fails, so it has no retries and no on_failure"))
		}
	} else if step.GetRetries() == 0 && kind != millv1.StepKind_STEP_KIND_CHECK {
		step.Retries = defaultRetries
	}
	if s.TokenBudget < 0 {
		return fail(fmt.Errorf("token_budget: %d is not a count of tokens", s.TokenBudget))
	}
	for _, model := range s.Models {
		if strings.TrimSpace(model) == "" {
			return fail(fmt.Errorf("models: an entry is empty; name the model, such as sonnet or claude-haiku-4-5"))
		}
	}
	for _, c := range s.Capabilities {
		capability, err := parseEnum[millv1.Capability](c, "capability")
		if err != nil {
			return fail(err)
		}
		step.Capabilities = append(step.Capabilities, capability)
	}
	if s.Skill != "" {
		// The digest is filled in by Load once the skill is read; the
		// document names only what a human can write.
		step.Skill = &millv1.SkillRef{Name: s.Skill}
	}
	if s.Check != nil {
		check := &millv1.Check{
			Command:      s.Check.Command,
			Exit:         s.Check.Exit,
			StdoutSchema: s.Check.StdoutSchema,
		}
		if check.Stdout, err = parseEnum[millv1.StdoutShape](s.Check.Stdout, "stdout"); err != nil {
			return fail(err)
		}
		if check.Feedback, err = parseEnum[millv1.Feedback](s.Check.Feedback, "feedback"); err != nil {
			return fail(err)
		}
		step.Check = check
	}
	if s.Artifact != nil {
		step.Artifact = &millv1.ArtifactSource{Path: s.Artifact.Path}
	}
	if s.Gate != nil {
		step.Gate = &millv1.Gate{
			Ask:        &millv1.Ask{Title: s.Gate.Ask.Title, Body: s.Gate.Ask.Body},
			Auto:       string(s.Gate.Auto),
			DenyResult: s.Gate.DenyResult,
		}
	}
	if len(s.Harness) > 0 {
		step.Harness = map[string]*millv1.HarnessSettings{}
		for name, options := range s.Harness {
			if name == retiredHarnessName {
				return fail(fmt.Errorf("harness.%s: the adapter is named %s; rename the key to harness.%s",
					retiredHarnessName, harnessName, harnessName))
			}
			document, err := structpb.NewStruct(options)
			if err != nil {
				return fail(fmt.Errorf("harness.%s: the options are not a JSON document: %w", name, err))
			}
			step.Harness[name] = &millv1.HarnessSettings{Options: document}
		}
	}
	if kind != millv1.StepKind_STEP_KIND_PREPARATORY {
		if step.OnFailure, err = parseOnFailure(s.OnFailure); err != nil {
			return fail(err)
		}
	}
	if s.Timeout != "" {
		d, err := time.ParseDuration(s.Timeout)
		if err != nil {
			return fail(fmt.Errorf("timeout: %w", err))
		}
		if d <= 0 {
			return fail(fmt.Errorf("timeout: %s is not a positive duration", s.Timeout))
		}
		step.Timeout = durationpb.New(d)
	}
	return step, nil
}

// parseOnFailure reads `park` or `revise <step>`. Anything else is a
// definition error: there is no branching in a workflow except this.
func parseOnFailure(text string) (*millv1.OnFailure, error) {
	switch fields := strings.Fields(text); {
	case len(fields) == 0:
		return &millv1.OnFailure{Kind: millv1.OnFailureKind_ON_FAILURE_KIND_PARK}, nil
	case len(fields) == 1 && fields[0] == "park":
		return &millv1.OnFailure{Kind: millv1.OnFailureKind_ON_FAILURE_KIND_PARK}, nil
	case len(fields) == 2 && fields[0] == "revise":
		return &millv1.OnFailure{
			Kind:         millv1.OnFailureKind_ON_FAILURE_KIND_REVISE,
			ReviseTarget: fields[1],
		}, nil
	default:
		return nil, fmt.Errorf("on_failure: %q is not park or revise <step>", text)
	}
}

// toDocument is toProto backwards: what `mill workflow pull` writes.
func toDocument(w *millv1.Workflow) document {
	doc := document{Name: w.GetName(), Scope: enumText(w.GetScope()), Project: w.GetProject()}
	if status := w.GetStatus(); status != nil {
		doc.Status = &statusYAML{
			Interval: status.GetInterval().AsDuration().String(),
			Timeout:  status.GetTimeout().AsDuration().String(),
		}
	}
	for _, step := range w.GetSteps() {
		s := stepYAML{
			Name:         step.GetName(),
			Kind:         enumText(step.GetKind()),
			Session:      step.GetSession(),
			Skill:        step.GetSkill().GetName(),
			Evidence:     enumText(step.GetEvidence()),
			Retries:      step.GetRetries(),
			OutputSchema: step.GetOutputSchema(),
			OnFailure:    onFailureText(step.GetOnFailure()),
			TokenBudget:  step.GetTokenBudget(),
			Models:       step.GetModels(),
		}
		for _, capability := range step.GetCapabilities() {
			s.Capabilities = append(s.Capabilities, enumText(capability))
		}
		if check := step.GetCheck(); check != nil {
			s.Check = &checkYAML{
				Command:      check.GetCommand(),
				Exit:         check.GetExit(),
				Stdout:       enumText(check.GetStdout()),
				StdoutSchema: check.GetStdoutSchema(),
				Feedback:     enumText(check.GetFeedback()),
			}
		}
		if artifact := step.GetArtifact(); artifact != nil {
			s.Artifact = &artifactYAML{Path: artifact.GetPath()}
		}
		if gate := step.GetGate(); gate != nil {
			s.Gate = &gateYAML{
				Ask:        askYAML{Title: gate.GetAsk().GetTitle(), Body: gate.GetAsk().GetBody()},
				Auto:       predicate(gate.GetAuto()),
				DenyResult: gate.GetDenyResult(),
			}
		}
		if len(step.GetHarness()) > 0 {
			s.Harness = map[string]map[string]any{}
			for name, settings := range step.GetHarness() {
				s.Harness[name] = settings.GetOptions().AsMap()
			}
		}
		if step.GetTimeout() != nil {
			s.Timeout = step.GetTimeout().AsDuration().String()
		}
		doc.Steps = append(doc.Steps, s)
	}
	return doc
}

func onFailureText(f *millv1.OnFailure) string {
	switch {
	case f == nil:
		// A preparatory step, which has none.
		return ""
	case f.GetKind() == millv1.OnFailureKind_ON_FAILURE_KIND_REVISE:
		return "revise " + f.GetReviseTarget()
	default:
		return "park"
	}
}

// parseEnum reads an enum by its lower-case name without the type prefix,
// which is how every enum is spelled in workflow.yaml. An unknown value
// names the field, because a typo in a definition should say where it is.
func parseEnum[E interface {
	~int32
	protoreflect.Enum
}](text, field string) (E, error) {
	var zero E
	if text == "" {
		return zero, nil
	}
	descriptor := zero.Descriptor()
	name := enumPrefix(descriptor) + strings.ToUpper(strings.ReplaceAll(text, "-", "_"))
	value := descriptor.Values().ByName(protoreflect.Name(name))
	if value == nil {
		return zero, fmt.Errorf("%s: %q is not one of %s", field, text, enumNames(descriptor))
	}
	return E(value.Number()), nil
}

// enumText is parseEnum backwards, and the zero value has no text.
func enumText[E interface {
	~int32
	protoreflect.Enum
}](value E) string {
	if value == 0 {
		return ""
	}
	descriptor := value.Descriptor()
	name := descriptor.Values().ByNumber(protoreflect.EnumNumber(value))
	if name == nil {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(string(name.Name()), enumPrefix(descriptor)))
}

// enumNames lists what a field will accept, for the error that says a value
// is not one of them.
func enumNames(descriptor protoreflect.EnumDescriptor) string {
	values := descriptor.Values()
	names := make([]string, 0, values.Len())
	for i := 1; i < values.Len(); i++ {
		name := strings.TrimPrefix(string(values.Get(i).Name()), enumPrefix(descriptor))
		names = append(names, strings.ToLower(name))
	}
	return strings.Join(names, ", ")
}

// enumPrefix is the prefix proto style gives every value of an enum: the
// type name in upper snake case, with a trailing underscore.
func enumPrefix(descriptor protoreflect.EnumDescriptor) string {
	var b strings.Builder
	for i, r := range string(descriptor.Name()) {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	b.WriteByte('_')
	return b.String()
}
