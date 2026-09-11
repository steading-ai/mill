package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// The ledger holds enums as text, because a row is meant to read on its own
// in psql and because a CHECK constraint is where the permitted set belongs.
// The text is the proto value name with its type's prefix removed, lowered:
// RUN_STATUS_SELECTING is `selecting`, TIER_QUICK_FIX is `quick_fix`. The
// two functions below are the whole mapping, so a value added to a proto
// enum needs only the CHECK constraint widened.

// enumText renders a proto enum as the ledger's text. The unspecified zero
// value renders as the empty string, which every nullable enum column
// stores as NULL.
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

// parseEnum reads the ledger's text back. Text no value matches, including
// the empty string, is the unspecified zero value.
func parseEnum[E interface {
	~int32
	protoreflect.Enum
}](text string) E {
	var zero E
	if text == "" {
		return zero
	}
	descriptor := zero.Descriptor()
	value := descriptor.Values().ByName(protoreflect.Name(enumPrefix(descriptor) + strings.ToUpper(text)))
	if value == nil {
		return zero
	}
	return E(value.Number())
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

// enumTexts renders a repeated enum field, for a text[] column.
func enumTexts[E interface {
	~int32
	protoreflect.Enum
}](values []E) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, enumText(v))
	}
	return out
}

// parseEnums reads a text[] column back.
func parseEnums[E interface {
	~int32
	protoreflect.Enum
}](texts []string) []E {
	out := make([]E, 0, len(texts))
	for _, t := range texts {
		out = append(out, parseEnum[E](t))
	}
	return out
}

// text renders an optional text column, treating NULL as empty.
func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// nullable is the inverse: the empty string is stored as NULL, so a column
// carries either a meaningful value or nothing.
func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// stamp renders a timestamp column, treating NULL as unset.
func stamp(value *time.Time) *timestamppb.Timestamp {
	if value == nil {
		return nil
	}
	return timestamppb.New(*value)
}

// runProto builds the wire form of a run row.
func runProto(row Run) *millv1.Run {
	return &millv1.Run{
		Id:              row.ID,
		NodeId:          row.NodeID,
		NodeRef:         row.NodeRef,
		Generation:      row.Generation,
		ContextId:       row.ContextID,
		WorkflowId:      text(row.WorkflowID),
		Status:          parseEnum[millv1.RunStatus](row.Status),
		LeaseId:         text(row.LeaseID),
		WorkerRef:       row.WorkerRef,
		ParkedReason:    parseEnum[millv1.ParkedReason](text(row.ParkedReason)),
		GateNodeId:      text(row.GateNodeID),
		CancelRequested: row.CancelRequested,
		Branch:          row.Branch,
		Tier:            parseEnum[millv1.Tier](text(row.Tier)),
		CreatedAt:       timestamppb.New(row.CreatedAt),
		UpdatedAt:       timestamppb.New(row.UpdatedAt),
		EndedAt:         stamp(row.EndedAt),

		CompleteRequestId: row.CompleteRequestID,
	}
}

// stepRunProto builds the wire form of a step run row.
func stepRunProto(row StepRun) (*millv1.StepRun, error) {
	exit := &harnessv1.ProcessExit{}
	if len(row.Exit) > 0 {
		if err := protojson.Unmarshal(row.Exit, exit); err != nil {
			return nil, fmt.Errorf("step run %s: decode exit: %w", row.ID, err)
		}
	} else {
		exit = nil
	}
	return &millv1.StepRun{
		Id:           row.ID,
		RunId:        row.RunID,
		StepName:     row.StepName,
		Attempt:      row.Attempt,
		Kind:         parseEnum[millv1.StepKind](row.Kind),
		SessionId:    text(row.SessionID),
		LeaseId:      text(row.LeaseID),
		WorkerRef:    row.WorkerRef,
		Status:       parseEnum[millv1.StepRunStatus](row.Status),
		Exit:         exit,
		FeedbackFrom: text(row.FeedbackFrom),
		AutoApproved: row.AutoApproved,
		StartedAt:    timestamppb.New(row.StartedAt),
		EndedAt:      stamp(row.EndedAt),

		EnvPassthrough: row.EnvPassthrough,
		EnvStripped:    row.EnvStripped,

		BaseCommit:     row.BaseCommit,
		SessionStarted: row.SessionStarted,
	}, nil
}

// stepRunProtos converts a batch, so a caller that failed on one row fails
// on the batch.
func stepRunProtos(rows []StepRun) ([]*millv1.StepRun, error) {
	out := make([]*millv1.StepRun, 0, len(rows))
	for _, row := range rows {
		stepRun, err := stepRunProto(row)
		if err != nil {
			return nil, err
		}
		out = append(out, stepRun)
	}
	return out, nil
}

// evidenceValue is the `value` column: the fact alone, without the columns
// that carry the rest of the row. The kind, the step run and the
// verification are columns, so a row reads without unpacking the JSON.
func evidenceValue(evidence *millv1.Evidence) ([]byte, error) {
	fact := &millv1.Evidence{}
	proto.Merge(fact, evidence)
	fact.StepRunId = ""
	fact.Kind = millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED
	fact.VerifiedBy = ""
	fact.VerifiedAt = nil
	value, err := protojson.Marshal(fact)
	if err != nil {
		return nil, fmt.Errorf("encode evidence value: %w", err)
	}
	return value, nil
}

// evidenceProto builds the wire form of an evidence row.
func evidenceProto(row Evidence) (*millv1.Evidence, error) {
	evidence := &millv1.Evidence{}
	if err := protojson.Unmarshal(row.Value, evidence); err != nil {
		return nil, fmt.Errorf("evidence for step run %s: decode value: %w", row.StepRunID, err)
	}
	evidence.StepRunId = row.StepRunID
	evidence.Kind = parseEnum[millv1.EvidenceKind](row.Kind)
	evidence.VerifiedBy = row.VerifiedBy
	evidence.VerifiedAt = timestamppb.New(row.VerifiedAt)
	return evidence, nil
}

// stepTokenProto builds the wire form of a step token row. The bearer is
// not here: it exists only in the response of the call that minted it.
func stepTokenProto(row StepToken) *millv1.StepToken {
	return &millv1.StepToken{
		Id:           row.ID,
		StepRunId:    row.StepRunID,
		TokenHash:    row.TokenHash,
		Capabilities: parseEnums[millv1.Capability](row.Capabilities),
		IssuedAt:     timestamppb.New(row.IssuedAt),
		RevokedAt:    stamp(row.RevokedAt),
	}
}

// usageProto builds the wire form of a usage row.
func usageProto(row Usage) *millv1.Usage {
	return &millv1.Usage{
		StepRunId:        row.StepRunID,
		Model:            row.Model,
		InputTokens:      row.InputTokens,
		OutputTokens:     row.OutputTokens,
		CacheReadTokens:  row.CacheReadTokens,
		CacheWriteTokens: row.CacheWriteTokens,
		RecordedAt:       timestamppb.New(row.RecordedAt),
		PostedAt:         stamp(row.PostedAt),
		Source:           usageSourceProto(row.Source),
	}
}

// The ledger's text for who counted a usage row (migration 00018).
const (
	usageSourceSDK     = "sdk"
	usageSourceGateway = "gateway"
)

// usageSourceText is a source as the ledger spells it. Unspecified is the
// adapter's: every row written before the gateway existed was its report.
func usageSourceText(source millv1.UsageSource) string {
	if source == millv1.UsageSource_USAGE_SOURCE_GATEWAY {
		return usageSourceGateway
	}
	return usageSourceSDK
}

// usageSourceProto is the ledger's text as the wire spells it.
func usageSourceProto(text string) millv1.UsageSource {
	if text == usageSourceGateway {
		return millv1.UsageSource_USAGE_SOURCE_GATEWAY
	}
	return millv1.UsageSource_USAGE_SOURCE_SDK
}

// sessionProto builds the wire form of a session row.
func sessionProto(row Session) (*millv1.Session, error) {
	adapterRef := &structpb.Struct{}
	if len(row.AdapterRef) > 0 {
		if err := protojson.Unmarshal(row.AdapterRef, adapterRef); err != nil {
			return nil, fmt.Errorf("session %s: decode adapter_ref: %w", row.ID, err)
		}
	}
	return &millv1.Session{
		RunId:      row.RunID,
		Name:       row.Name,
		Id:         row.ID,
		Harness:    row.Harness,
		AdapterRef: adapterRef,
		WorkerRef:  row.WorkerRef,
		StartedAt:  timestamppb.New(row.StartedAt),
		LastTurnAt: stamp(row.LastTurnAt),
	}, nil
}

// workflowProto rebuilds a workflow from the definition it was stored as,
// with the columns that identify the version applied over it.
func workflowProto(row Workflow) (*millv1.Workflow, error) {
	workflow := &millv1.Workflow{}
	if err := protojson.Unmarshal(row.Definition, workflow); err != nil {
		return nil, fmt.Errorf("workflow %s: decode definition: %w", row.ID, err)
	}
	source := &millv1.Source{}
	if len(row.Source) > 0 {
		if err := protojson.Unmarshal(row.Source, source); err != nil {
			return nil, fmt.Errorf("workflow %s: decode source: %w", row.ID, err)
		}
	}
	workflow.Id = row.ID
	workflow.Name = row.Name
	workflow.Version = row.Version
	workflow.Scope = parseEnum[millv1.Scope](row.Scope)
	workflow.Project = row.Project
	workflow.Source = source
	workflow.CreatedAt = timestamppb.New(row.CreatedAt)
	return workflow, nil
}

// skillProto builds the wire form of a skill row.
func skillProto(row Skill) (*millv1.Skill, error) {
	var files, metadata map[string]string
	if err := json.Unmarshal(row.Files, &files); err != nil {
		return nil, fmt.Errorf("skill %s: decode files: %w", row.ID, err)
	}
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return nil, fmt.Errorf("skill %s: decode metadata: %w", row.ID, err)
	}
	source := &millv1.Source{}
	if len(row.Source) > 0 {
		if err := protojson.Unmarshal(row.Source, source); err != nil {
			return nil, fmt.Errorf("skill %s: decode source: %w", row.ID, err)
		}
	}
	return &millv1.Skill{
		Id:          row.ID,
		Name:        row.Name,
		Digest:      row.Digest,
		Description: row.Description,
		Body:        row.Body,
		Files:       files,
		Metadata:    metadata,
		Source:      source,
		CreatedAt:   timestamppb.New(row.CreatedAt),
	}, nil
}

// jsonbMessage encodes a proto message for a jsonb column, rendering a nil
// message as an empty object rather than as SQL NULL.
func jsonbMessage(message proto.Message) ([]byte, error) {
	if message == nil || !message.ProtoReflect().IsValid() {
		return []byte("{}"), nil
	}
	return protojson.Marshal(message)
}

// jsonbMap encodes a string map for a jsonb column.
func jsonbMap(values map[string]string) ([]byte, error) {
	if values == nil {
		values = map[string]string{}
	}
	return json.Marshal(values)
}

// eventProto builds the wire form of an audit-trail row. The before and
// after columns are free-form JSON objects the writing call composed, so
// they travel as structs rather than as a typed shape nothing would agree
// on.
func eventProto(row Event) (*millv1.Event, error) {
	before, err := structFrom(row.Before)
	if err != nil {
		return nil, fmt.Errorf("event %s: decode before: %w", row.ID, err)
	}
	after, err := structFrom(row.After)
	if err != nil {
		return nil, fmt.Errorf("event %s: decode after: %w", row.ID, err)
	}
	return &millv1.Event{
		Id:         row.ID,
		RunId:      row.RunID,
		StepRunId:  text(row.StepRunID),
		Op:         row.Op,
		Before:     before,
		After:      after,
		WorkerRef:  row.WorkerRef,
		OccurredAt: timestamppb.New(row.OccurredAt),
	}, nil
}

// structFrom decodes a nullable jsonb column into a struct, treating NULL and
// the empty column as no value at all.
func structFrom(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	value := &structpb.Struct{}
	if err := protojson.Unmarshal(raw, value); err != nil {
		return nil, err
	}
	return value, nil
}
