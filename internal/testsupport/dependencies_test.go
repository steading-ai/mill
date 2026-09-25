package testsupport_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
)

// goArchive is the tar extractor testcontainers-go pulls in. Below v0.3.0 a
// crafted archive can write outside its extraction directory
// (CVE-2026-17106), so this package, the only path that reaches it, pins the
// floor.
const goArchive = "github.com/moby/go-archive"

// moduleVersion is the version go.mod requires for path. The build info
// linked into a test binary lists only the packages it reached, and
// testcontainers reaches the archive code lazily, so go.mod is the record.
func moduleVersion(path string) string {
	GinkgoHelper()
	f, err := os.Open(filepath.Join("..", "..", "go.mod"))
	Expect(err).NotTo(HaveOccurred())
	defer f.Close() //nolint:errcheck // read only
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) >= 2 && fields[0] == path {
			return fields[1]
		}
	}
	Expect(lines.Err()).NotTo(HaveOccurred())
	Fail(path + " is not in go.mod")
	return ""
}

// majorMinor splits a "vMAJOR.MINOR.PATCH" module version into its leading
// two numbers.
func majorMinor(v string) (int, int) {
	GinkgoHelper()
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	Expect(parts).To(HaveLen(3), "not a semantic version: %q", v)
	major, err := strconv.Atoi(parts[0])
	Expect(err).NotTo(HaveOccurred())
	minor, err := strconv.Atoi(parts[1])
	Expect(err).NotTo(HaveOccurred())
	return major, minor
}

var _ = Describe("Dependencies", func() {
	It("The vulnerable module is gone from the graph", func() {
		v := moduleVersion(goArchive)
		major, minor := majorMinor(v)
		Expect(major > 0 || minor >= 3).To(BeTrue(), "%s is %s; CVE-2026-17106 is fixed in v0.3.0", goArchive, v)
	})

	It("The database suite still runs", Label(testsupport.DBLabel), func(ctx SpecContext) {
		var one int
		Expect(pg.Database(ctx).QueryRow(ctx, `SELECT 1`).Scan(&one)).To(Succeed())
		Expect(one).To(Equal(1))
	})
})
