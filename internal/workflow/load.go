package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// Load reads a workflow directory into the definition the store holds and
// the skills its steps pin.
//
//	<dir>/workflow.yaml
//	<dir>/skills/<name>/SKILL.md
//
// Every skill step's skill is read, hashed, and pinned by (name, digest) on
// the step, so the version Load computes changes when any prompt in the
// workflow changes. The definition is then validated: a workflow that would
// fail mid-run is refused here instead. See [Validate].
func Load(dir string) (*millv1.Workflow, []*millv1.Skill, error) {
	content, err := os.ReadFile(filepath.Join(dir, DefinitionFile))
	if err != nil {
		return nil, nil, err
	}
	doc, err := decode(content)
	if err != nil {
		return nil, nil, err
	}
	w, err := doc.toProto()
	if err != nil {
		return nil, nil, err
	}
	if w.GetName() == "" {
		return nil, nil, fmt.Errorf("%s: name is required", DefinitionFile)
	}

	// One skill may serve several steps; it is read and stored once, and
	// every step that names it pins the same digest.
	loaded := map[string]*millv1.Skill{}
	var skills []*millv1.Skill
	for _, step := range w.GetSteps() {
		reference := step.GetSkill().GetName()
		if reference == "" {
			continue
		}
		skill, ok := loaded[reference]
		if !ok {
			path, err := skillDir(dir, reference)
			if err != nil {
				return nil, nil, fmt.Errorf("step %s: %w", step.GetName(), err)
			}
			if skill, err = LoadSkill(path); err != nil {
				return nil, nil, fmt.Errorf("step %s: %w", step.GetName(), err)
			}
			loaded[reference] = skill
			skills = append(skills, skill)
		}
		// The step pins the skill's own name, never the reference that
		// found it: a borrowed skill is the same record as the one its
		// own workflow pins, and the store keys it by (name, digest).
		step.Skill = &millv1.SkillRef{Name: skill.GetName(), Digest: skill.GetDigest()}
	}

	for _, step := range w.GetSteps() {
		schema, err := outputSchema(dir, step)
		if err != nil {
			return nil, nil, fmt.Errorf("step %s: %w", step.GetName(), err)
		}
		step.OutputSchema = schema
	}

	if err := Validate(w); err != nil {
		return nil, nil, err
	}
	if w.Version, err = Version(w); err != nil {
		return nil, nil, err
	}
	return w, skills, nil
}

// skillDir resolves a step's `skill` reference to the directory holding its
// SKILL.md.
//
// A bare name is this workflow's own skill, `<dir>/skills/<name>`.
// `<workflow>/<name>` is a sibling workflow's, `<dir>/../<workflow>/skills/<name>`
// — how `feature` reuses the skills `quick-fix` already carries rather than
// keeping a second copy of them to drift. The reference is two plain path
// segments and nothing else: a definition that reached out of the workflows
// directory would pin a skill nobody pushing this repository has.
func skillDir(dir, reference string) (string, error) {
	segments := strings.Split(reference, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsRune(segment, filepath.Separator) {
			return "", fmt.Errorf("skill %q: a reference is a name, or a sibling workflow's name and a name", reference)
		}
	}
	switch len(segments) {
	case 1:
		return filepath.Join(dir, SkillsDir, segments[0]), nil
	case 2:
		return filepath.Join(dir, "..", segments[0], SkillsDir, segments[1]), nil
	default:
		return "", fmt.Errorf("skill %q: a reference is a name, or a sibling workflow's name and a name", reference)
	}
}

// Write is Load backwards: the definition and its skills as a directory
// Load accepts. `mill workflow pull` is a call to this.
func Write(dir string, w *millv1.Workflow, skills []*millv1.Skill) error {
	content, err := encode(toDocument(w))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, DefinitionFile), content, 0o644); err != nil {
		return err
	}
	for _, skill := range skills {
		if err := WriteSkill(filepath.Join(dir, SkillsDir, skill.GetName()), skill); err != nil {
			return err
		}
	}
	return nil
}

// outputSchema is a step's `output_schema`, which a definition writes either
// inline or as the name of a file in the workflow directory. A schema long
// enough to be worth reading — the verdict a reviewer answers with — reads
// as a schema in a `.json` file and as noise in the middle of a YAML step,
// and a file is also what a session's own tooling can be pointed at.
//
// The file's content is stored on the step, so the store still holds one
// self-contained definition and the workflow's version changes when the
// schema does.
func outputSchema(dir string, step *millv1.Step) (string, error) {
	declared := strings.TrimSpace(step.GetOutputSchema())
	if declared == "" || strings.HasPrefix(declared, "{") {
		return step.GetOutputSchema(), nil
	}
	if filepath.IsAbs(declared) {
		return "", fmt.Errorf("output_schema %q is absolute; it is a file in the workflow directory", declared)
	}
	for _, segment := range strings.Split(filepath.ToSlash(declared), "/") {
		if segment == ".." {
			return "", fmt.Errorf("output_schema %q climbs out of the workflow directory", declared)
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(declared)))
	if err != nil {
		return "", fmt.Errorf("output_schema: %w", err)
	}
	return string(content), nil
}
