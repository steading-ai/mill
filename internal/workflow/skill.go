package workflow

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// frontmatterFields is the whole normative surface of the agent-skills
// spec: six fields, and `metadata` string to string only. A skill carrying
// anything else is refused here for the same reason the reference validator
// refuses it — a loader that tolerates an extra key today is a loader that
// has to keep tolerating it, and it is exactly where mill-specific
// orchestration would start to leak (docs/core-principles.md §6).
var frontmatterFields = []string{
	"allowed-tools",
	"compatibility",
	"description",
	"license",
	"metadata",
	"name",
}

// bundleDirs are the directories a skill may carry beside SKILL.md. Their
// contents are hashed into the manifest, path to digest.
var bundleDirs = []string{"assets", "references", "scripts"}

// frontmatter is what a SKILL.md declares about itself.
type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	AllowedTools  []string          `yaml:"allowed-tools,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
}

// LoadSkill reads one skill directory: SKILL.md's frontmatter and prompt,
// plus a manifest of whatever it carries in assets/, references/, and
// scripts/. The digest it returns is the one a step references and the one
// the store would compute for the same record.
//
// A skill is used as a prompt, never installed (docs/architecture.md
// §"Skill steps"), so nothing here resolves an `allowed-tools` entry or a
// relative file reference. That is the adapter's business, and only for the
// harness it is talking to.
func LoadSkill(dir string) (*millv1.Skill, error) {
	name := filepath.Base(dir)
	content, err := os.ReadFile(filepath.Join(dir, SkillFile))
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", name, err)
	}
	front, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", name, err)
	}
	if front.Name != name {
		return nil, fmt.Errorf("skill %s: frontmatter names it %q; the directory and the name must agree", name, front.Name)
	}
	files, err := manifest(dir)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", name, err)
	}
	skill := &millv1.Skill{
		Name:        front.Name,
		Description: front.Description,
		Body:        body,
		Files:       files,
		Metadata:    front.Metadata,
	}
	if skill.Digest, err = Digest(skill); err != nil {
		return nil, fmt.Errorf("skill %s: %w", name, err)
	}
	return skill, nil
}

// parseFrontmatter splits a SKILL.md into its declared fields and its
// prompt. The prompt is the body with surrounding whitespace removed, which
// is what makes a skill written by hand and a skill written by
// `mill workflow pull` hash the same.
func parseFrontmatter(content []byte) (frontmatter, string, error) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return frontmatter{}, "", fmt.Errorf("%s does not open with a --- frontmatter block", SkillFile)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return frontmatter{}, "", fmt.Errorf("%s has no closing --- for its frontmatter", SkillFile)
	}
	head, body := rest[:end+1], strings.TrimPrefix(rest[end+len("\n---"):], "\n")

	if err := checkFields(head); err != nil {
		return frontmatter{}, "", err
	}
	var front frontmatter
	dec := yaml.NewDecoder(strings.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&front); err != nil {
		return frontmatter{}, "", fmt.Errorf("frontmatter: %w", err)
	}
	if front.Name == "" {
		return frontmatter{}, "", fmt.Errorf("frontmatter: name is required")
	}
	if front.Description == "" {
		return frontmatter{}, "", fmt.Errorf("frontmatter: description is required")
	}
	return front, strings.TrimSpace(body), nil
}

// checkFields refuses a frontmatter key outside the six the spec reserves,
// with the message the reference validator gives, so a skill that fails
// here fails `skills-ref validate` too.
func checkFields(head string) error {
	var declared map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(head), &declared); err != nil {
		return fmt.Errorf("frontmatter: %w", err)
	}
	unexpected := make([]string, 0, len(declared))
	for key := range declared {
		if !slices.Contains(frontmatterFields, key) {
			unexpected = append(unexpected, key)
		}
	}
	if len(unexpected) > 0 {
		slices.Sort(unexpected)
		return fmt.Errorf("unexpected fields in frontmatter: %s. Only [%s] are allowed",
			strings.Join(unexpected, ", "), strings.Join(quoted(frontmatterFields), ", "))
	}
	return nil
}

// manifest hashes everything a skill carries beside SKILL.md: path relative
// to the skill directory, to the SHA-256 of its bytes. Slash-separated, so
// a manifest is the same on every platform and so is the digest over it.
func manifest(dir string) (map[string]string, error) {
	files := map[string]string{}
	for _, sub := range bundleDirs {
		root := filepath.Join(dir, sub)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = fileDigest(content)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read %s/: %w", sub, err)
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	return files, nil
}

// WriteSkill writes a skill back out as a SKILL.md `mill workflow push`
// accepts. It is the inverse of [LoadSkill] for everything the store keeps:
// a skill that carried files cannot be written back, because the store
// holds their digests and not their bytes.
func WriteSkill(dir string, skill *millv1.Skill) error {
	if len(skill.GetFiles()) > 0 {
		return fmt.Errorf("skill %s carries %d file(s); the store keeps their digests, not their bytes",
			skill.GetName(), len(skill.GetFiles()))
	}
	head, err := yaml.Marshal(frontmatter{
		Name:        skill.GetName(),
		Description: skill.GetDescription(),
		Metadata:    skill.GetMetadata(),
	})
	if err != nil {
		return fmt.Errorf("skill %s: encode frontmatter: %w", skill.GetName(), err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("skill %s: %w", skill.GetName(), err)
	}
	content := "---\n" + string(head) + "---\n\n" + skill.GetBody() + "\n"
	if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte(content), 0o644); err != nil {
		return fmt.Errorf("skill %s: %w", skill.GetName(), err)
	}
	return nil
}

func quoted(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, "'"+v+"'")
	}
	return out
}
