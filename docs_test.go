package mill_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const adrDir = "docs/adrs"

// adrWordLimit is the ceiling mill#7 sets: an ADR a future agent reads before
// changing a decision is worth nothing if it is longer than the design it records.
const adrWordLimit = 600

var (
	adrFileName = regexp.MustCompile(`^\d{3}-[a-z0-9-]+\.md$`)
	// A citation of a design document: a link out of docs/adrs/ into docs/,
	// naming a section rather than a whole file.
	adrCitation = regexp.MustCompile(`\]\(\.\./[a-z-]+\.md#[^)]+\)`)
	// A rejected alternative is a subheading under Rejected Options.
	adrSubheading = regexp.MustCompile(`(?m)^### .+$`)
)

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
	It("numbers seventeen of them, 001 through 017", func() {
		names := adrFiles()

		var numbers []string
		for _, name := range names {
			numbers = append(numbers, name[:3])
		}

		Expect(numbers).To(Equal([]string{
			"001", "002", "003", "004", "005", "006",
			"007", "008", "009", "010", "011", "012",
			"013", "014", "015", "016", "017",
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
