// Package workflow turns a directory of files into the versioned definition
// the store holds.
//
// A workflow is data: `workflow.yaml` beside a `skills/` directory, one
// `SKILL.md` per skill step. The split is docs/core-principles.md §6 — the
// skill is the portable part and carries nothing mill-specific, so every
// ordering decision, evidence rule, gate, and harness setting lives in
// `workflow.yaml` and never in frontmatter. docs/domain-model.md §Workflow
// is the model this loads into.
//
// Loading is also where a definition is refused. The rules the rest of the
// design leans on — a `revise` that goes backwards, a branch on the forge
// before any gate, unique step names, a skill that exists — are checked here
// so a broken workflow fails at push rather than mid-run. See [Validate].
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// Layout of a workflow directory.
const (
	// DefinitionFile is the mill record: the steps, in order, and everything
	// about them the agent-skills spec has no room for.
	DefinitionFile = "workflow.yaml"
	// SkillsDir holds one directory per skill, each with a SKILL.md.
	SkillsDir = "skills"
	// SkillFile is the skill itself: frontmatter, then the prompt.
	SkillFile = "SKILL.md"
)

// Version is the content hash that identifies a workflow version: two
// definitions that hash the same are the same version, and a run pins the
// one it started with.
//
// The fields that identify a stored row rather than describe the definition
// — id, version, source, creation time — are excluded, so pushing the same
// files from two checkouts yields one version. The store computes the same
// hash the same way; a loader that disagreed with it would create a version
// per push.
func Version(w *millv1.Workflow) (string, error) {
	canonical := proto.Clone(w).(*millv1.Workflow)
	canonical.Id = ""
	canonical.Version = ""
	canonical.Source = nil
	canonical.CreatedAt = nil
	return contentHash(canonical)
}

// Digest is the content hash of a skill: its name, description, prompt,
// file manifest, and metadata. A byte changed in the body is a new digest,
// and because a step references a skill by (name, digest), it is a new
// workflow version too.
func Digest(s *millv1.Skill) (string, error) {
	canonical := proto.Clone(s).(*millv1.Skill)
	canonical.Id = ""
	canonical.Digest = ""
	canonical.Source = nil
	canonical.CreatedAt = nil
	return contentHash(canonical)
}

// contentHash is the SHA-256 of a message's deterministic binary encoding,
// hex. The store hashes the same way, which is what lets a loader hand it a
// digest it would have computed itself.
func contentHash(message proto.Message) (string, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("hash definition: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// fileDigest is the SHA-256 of bytes, hex. What the manifest of a skill's
// scripts, assets, and references records per path.
func fileDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
