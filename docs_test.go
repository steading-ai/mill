package mill_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const adrDir = "docs/adrs"

// adrWordLimit is the ceiling on a record: an ADR a future agent reads before
// changing a decision is worth nothing if it is longer than the design it
// records. Nine weighed backends fit under it; a tenth section does not.
const adrWordLimit = 900

// sandboxDocs are the documents the per-run sandbox design landed as. They
// arrived written for another tool's link syntax, so the specs hold them to
// this repository's: relative paths that resolve.
var sandboxDocs = []string{
	"docs/sandbox.md",
	"docs/sandbox-scenarios.md",
	"docs/adrs/018-a-runs-sandbox-is-a-systemd-nspawn-machine.md",
}

var (
	adrFileName = regexp.MustCompile(`^\d{3}-[a-z0-9-]+\.md$`)
	// A citation of a design document: a link out of docs/adrs/ into docs/,
	// naming a section rather than a whole file.
	adrCitation = regexp.MustCompile(`\]\(\.\./[a-z-]+\.md#[^)]+\)`)
	// A rejected alternative is a subheading under Rejected Options.
	adrSubheading = regexp.MustCompile(`(?m)^### .+$`)
	// A wiki-link: the vault's syntax, which resolves nowhere in this tree.
	wikiLink = regexp.MustCompile(`\[\[[^\]]+\]\]`)
	// A markdown link to a relative path, with or without a fragment.
	relativeLink = regexp.MustCompile(`\]\(([^):#\s]+)(#[^)]*)?\)`)
	// A deferral: a sentence that hands a fact to the spike, to "the
	// implementation", or to a later decision, instead of stating it.
	deferral = regexp.MustCompile(`(?i)the spike (reports|decides|will decide|is to decide|has to)|` +
		`the implementation (decides|will|should|is to)|the answer goes here|to be decided|` +
		`stays a spike|decides which|until (it|that) is run|for now|\bTBD\b|\bTODO\b|^- Whether\b`)
	// A repository path cited in backticks: rooted at a directory the repo map
	// names, so a bare filename or a path inside a machine is not one.
	repoPath = regexp.MustCompile("`((?:docs|internal|sidecar|workflows|contrib|tests|cmd|proto|gen|harness)/[^`\\s]*)`")
)

// citedPaths are the repository paths a document points at, resolved against
// the tree root and deduplicated.
func citedPaths(doc string) []string {
	source, err := os.ReadFile(doc)
	Expect(err).NotTo(HaveOccurred())

	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	for _, match := range relativeLink.FindAllStringSubmatch(string(source), -1) {
		add(filepath.Join(filepath.Dir(doc), match[1]))
	}
	for _, match := range repoPath.FindAllStringSubmatch(string(source), -1) {
		add(match[1])
	}
	return paths
}

func adrFiles() []string {
	entries, err := os.ReadDir(adrDir)
	Expect(err).NotTo(HaveOccurred())

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && adrFileName.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func adrBody(name string) string {
	source, err := os.ReadFile(filepath.Join(adrDir, name))
	Expect(err).NotTo(HaveOccurred())
	return string(source)
}

var _ = Describe("The architecture decision records", Label("unit"), func() {
	It("numbers eighteen of them, 001 through 018", func() {
		names := adrFiles()

		var numbers []string
		for _, name := range names {
			numbers = append(numbers, name[:3])
		}

		Expect(numbers).To(Equal([]string{
			"001", "002", "003", "004", "005", "006",
			"007", "008", "009", "010", "011", "012",
			"013", "014", "015", "016", "017", "018",
		}))
	})

	// mill#28: O1's own decisions, recorded when the milestone forced them.
	It("The decisions are on record", func() {
		var records []string
		for _, name := range adrFiles() {
			if number := name[:3]; number >= "013" && number <= "017" {
				records = append(records, name)
			}
		}

		Expect(records).To(HaveLen(5), "O1 files five records, 013 through 017")

		for _, name := range records {
			body := adrBody(name)

			Expect(len(strings.Fields(body))).To(BeNumerically("<", adrWordLimit), name)
			Expect(adrCitation.FindAllString(body, -1)).NotTo(BeEmpty(),
				"%s cites no section of a design document", name)

			Expect(body).To(ContainSubstring("## Rejected Options"), name)
			_, rejected, _ := strings.Cut(body, "## Rejected Options")
			rejected, _, _ = strings.Cut(rejected, "## More Information")
			Expect(adrSubheading.FindAllString(rejected, -1)).NotTo(BeEmpty(),
				"%s lists no rejected alternative", name)
		}
	})

	// mill#55: the sandbox design, written elsewhere, made this repository's.
	It("The design is on record in the repository's shape", func() {
		By("record 018 satisfies numbering, length, citation and rejected-option rules")
		var record string
		for _, name := range adrFiles() {
			if strings.HasPrefix(name, "018-") {
				record = name
			}
		}
		Expect(record).To(Equal("018-a-runs-sandbox-is-a-systemd-nspawn-machine.md"))

		body := adrBody(record)
		Expect(len(strings.Fields(body))).To(BeNumerically("<", adrWordLimit))
		Expect(adrCitation.FindAllString(body, -1)).NotTo(BeEmpty(),
			"018 cites no section of a design document")

		Expect(body).To(ContainSubstring("## Rejected Options"))
		_, rejected, _ := strings.Cut(body, "## Rejected Options")
		rejected, _, _ = strings.Cut(rejected, "## More Information")
		Expect(adrSubheading.FindAllString(rejected, -1)).NotTo(BeEmpty(),
			"018 lists no rejected alternative")

		index := adrBody("README.md")
		Expect(regexp.MustCompile(`(?m)^\|.*\(`+regexp.QuoteMeta(record)+`\).*\|$`).
			FindAllString(index, -1)).To(HaveLen(1), "018 should have exactly one row in the index")

		By("every path the sandbox documents cite exists")
		for _, doc := range sandboxDocs {
			source, err := os.ReadFile(doc)
			Expect(err).NotTo(HaveOccurred())
			Expect(wikiLink.FindAllString(string(source), -1)).To(BeEmpty(),
				"%s still carries a wiki-link", doc)

			paths := citedPaths(doc)
			Expect(paths).NotTo(BeEmpty(), "%s cites nothing in the tree", doc)
			for _, path := range paths {
				Expect(path).To(BeAnExistingFile(), "%s cites %s, which is not in the tree", doc, path)
			}
		}
	})

	// mill#70: the sandbox specification states what was built.
	It("The spec states facts", func() {
		source, err := os.ReadFile("docs/sandbox.md")
		Expect(err).NotTo(HaveOccurred())

		var deferrals []string
		for i, line := range strings.Split(string(source), "\n") {
			if match := deferral.FindString(line); match != "" {
				deferrals = append(deferrals, fmt.Sprintf("line %d: %q", i+1, match))
			}
		}
		Expect(deferrals).To(BeEmpty(), "docs/sandbox.md still defers instead of stating the fact")
	})

	It("keeps every record under the word limit", func() {
		for _, name := range adrFiles() {
			words := len(strings.Fields(adrBody(name)))
			Expect(words).To(BeNumerically("<", adrWordLimit),
				"%s is %d words", name, words)
		}
	})

	It("names at least one rejected alternative in every record", func() {
		for _, name := range adrFiles() {
			body := adrBody(name)
			Expect(body).To(ContainSubstring("## Rejected Options"), name)

			_, rejected, _ := strings.Cut(body, "## Rejected Options")
			rejected, _, _ = strings.Cut(rejected, "## More Information")
			Expect(adrSubheading.FindAllString(rejected, -1)).NotTo(BeEmpty(),
				"%s lists no rejected alternative", name)
		}
	})

	It("cites the section of the design document every record records", func() {
		for _, name := range adrFiles() {
			Expect(adrCitation.FindAllString(adrBody(name), -1)).NotTo(BeEmpty(),
				"%s cites no section of a design document", name)
		}
	})

	It("lists every record in the index with one line each", func() {
		index := adrBody("README.md")

		for _, name := range adrFiles() {
			rows := regexp.MustCompile(`(?m)^\|.*\(`+regexp.QuoteMeta(name)+`\).*\|$`).
				FindAllString(index, -1)
			Expect(rows).To(HaveLen(1), "%s should have exactly one row in the index", name)
			Expect(strings.Split(rows[0], "|")).To(HaveLen(4),
				"%s's row should be a link and one line", name)
		}
	})
})
