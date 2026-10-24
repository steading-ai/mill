package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// pseudoVersionCommit is the trailing 12-hex-character commit of a Go
// pseudo-version, e.g. the `460425ce6443` in
// `v0.0.0-20260925031338-460425ce6443`.
var pseudoVersionCommit = regexp.MustCompile(`-([0-9a-f]{12})$`)

// skiftLimit reads one `Max<Name>Bytes = <N> << 10` constant out of
// `internal/domain/limits.go`'s source text.
func skiftLimit(source, name string) (int, bool) {
	m := regexp.MustCompile(`Max` + name + `Bytes\s*=\s*(\d+)\s*<<\s*10`).FindStringSubmatch(source)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n << 10, true
}

var _ = Describe("The breaker's mirrored ceilings", func() {
	It("match skift's, at the commit mill's go.mod pins", func() {
		repo := testsupport.SkiftRepo()
		if repo == "" {
			Skip("no skift checkout: set " + testsupport.SkiftRepoEnv + " to check for drift")
		}
		if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
			Skip("skift checkout at " + repo + " has no .git: cannot read its history")
		}

		spawner := workspace.Spawner{Floor: testsupport.ScratchEnv()}
		ctx := context.Background()

		list, err := spawner.Exec(ctx, workspace.Command{
			Path: "go", Args: []string{"list", "-m", "-f", "{{.Version}}", "github.com/steading-ai/skift"},
		})
		Expect(err).NotTo(HaveOccurred())
		version, listErr := io.ReadAll(list.Stdout)
		diagnostics, _ := io.ReadAll(list.Stderr)
		result := list.Wait()
		Expect(listErr).NotTo(HaveOccurred())
		Expect(result.Status).To(BeZero(), "go list -m: %s", diagnostics)

		match := pseudoVersionCommit.FindStringSubmatch(strings.TrimSpace(string(version)))
		if match == nil {
			Skip("skift's pinned version " + strings.TrimSpace(string(version)) + " is not a pseudo-version")
		}
		commit := match[1]

		show, err := spawner.Exec(ctx, workspace.Command{
			Path: "git", Args: []string{"-c", "safe.directory=*", "show", commit + ":internal/domain/limits.go"},
			Dir: repo,
		})
		Expect(err).NotTo(HaveOccurred())
		source, sourceErr := io.ReadAll(show.Stdout)
		showDiagnostics, _ := io.ReadAll(show.Stderr)
		showResult := show.Wait()
		Expect(sourceErr).NotTo(HaveOccurred())
		if showResult.Status != 0 {
			Skip("skift checkout at " + repo + " does not have commit " + commit + " (e.g. a shallow clone): " + string(showDiagnostics))
		}

		comment, ok := skiftLimit(string(source), "Comment")
		Expect(ok).To(BeTrue(), "could not find MaxCommentBytes in limits.go at %s", commit)
		body, ok := skiftLimit(string(source), "Body")
		Expect(ok).To(BeTrue(), "could not find MaxBodyBytes in limits.go at %s", commit)

		Expect(maxCommentBytes).To(Equal(comment), "breaker.go's mirrored maxCommentBytes has drifted from skift's, at the commit mill's go.mod pins")
		Expect(maxBodyBytes).To(Equal(body), "breaker.go's mirrored maxBodyBytes has drifted from skift's, at the commit mill's go.mod pins")
	})
})
