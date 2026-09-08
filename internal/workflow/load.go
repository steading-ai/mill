package workflow

import (
	"fmt"
	"os"
	"path/filepath"

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
		name := step.GetSkill().GetName()
		if name == "" {
			continue
		}
		skill, ok := loaded[name]
		if !ok {
			if skill, err = LoadSkill(filepath.Join(dir, SkillsDir, name)); err != nil {
				return nil, nil, fmt.Errorf("step %s: %w", step.GetName(), err)
			}
			loaded[name] = skill
			skills = append(skills, skill)
		}
		step.Skill.Digest = skill.GetDigest()
	}

	if err := Validate(w); err != nil {
		return nil, nil, err
	}
	if w.Version, err = Version(w); err != nil {
		return nil, nil, err
	}
	return w, skills, nil
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
