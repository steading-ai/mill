package migrations_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/store/migrations"
	"github.com/steading-ai/mill/internal/testsupport"
)

// CheckOrder's own scenario numbers, run with no git and no database.
var _ = Describe("CheckOrder", func() {
	target := map[string]int64{
		"00028_step_runs_seq.sql":                28,
		"00029_exit_evidence_on_failed_step.sql": 29,
	}

	It("refuses a branch that adds a migration numbered at or below the target's highest", func() {
		ours := map[string]int64{
			"00028_step_runs_seq.sql":                28,
			"00029_exit_evidence_on_failed_step.sql": 29,
			"00028_another_thing.sql":                28,
		}
		violations := migrations.CheckOrder(target, ours)
		Expect(violations).To(ConsistOf(migrations.Violation{
			File: "00028_another_thing.sql", Version: 28, TargetHighest: 29,
		}))
		Expect(violations[0].Error()).To(ContainSubstring("renumber it to at least 30"))
	})

	It("passes a branch that adds a migration numbered above the target's highest", func() {
		ours := map[string]int64{
			"00028_step_runs_seq.sql":                28,
			"00029_exit_evidence_on_failed_step.sql": 29,
			"00030_something_new.sql":                30,
		}
		Expect(migrations.CheckOrder(target, ours)).To(BeEmpty())
	})
})

// The merge gate's catchUp (internal/run/gate.go) always runs a credentialed
// `git fetch origin <target>` before live-tier's `make test` runs, and
// because the clone it fetches into is a plain, non-single-branch clone,
// that fetch lands not only at FETCH_HEAD but at refs/remotes/origin/<target>
// under the default refspec. So this spec never fetches anything of its
// own: it reads what catchUp already refreshed, straight off disk, with no
// credential to have or lack — which is what lets it run inside live-tier's
// own `make test`.
var _ = Describe("The order check, against this checkout's own target", func() {
	It("finds no violation between this branch's migrations and the target's", func() {
		target, err := runGit("symbolic-ref", "-q", "refs/remotes/origin/HEAD")
		if err != nil || target == "" {
			requireOrderFixture("no refs/remotes/origin/HEAD symref to resolve a target from")
			return
		}

		listing, err := runGit("ls-tree", "-r", "--name-only", target, "--", "internal/store/migrations")
		if err != nil {
			requireOrderFixture("git ls-tree against " + target + ": " + err.Error())
			return
		}

		targetVersions := map[string]int64{}
		for _, path := range strings.Split(listing, "\n") {
			if path == "" {
				continue
			}
			if version, ok := migrations.VersionOf(filepath.Base(path)); ok {
				targetVersions[filepath.Base(path)] = version
			}
		}

		ours, err := migrations.Versions(migrations.FS)
		Expect(err).NotTo(HaveOccurred())

		Expect(migrations.CheckOrder(targetVersions, ours)).To(BeEmpty())
	})
})

// runGit runs a read-only, local git command: no fetch, no credential. The
// symref lookup below reads the local origin/HEAD a plain clone already
// sets — the same branch GitHub's own default-branch fallback picks when a
// run does not name a target (internal/forge/github/github.go).
func runGit(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// requireOrderFixture skips when this checkout cannot resolve a target,
// naming why — or fails under MILL_REQUIRE_FIXTURES, because live-tier sets
// that precisely so a target this check needed and didn't get is a failure,
// not a quiet pass (matching testsupport.requireAvailable's own convention).
func requireOrderFixture(reason string) {
	GinkgoHelper()
	if os.Getenv(testsupport.RequireFixturesEnv) != "" {
		Fail(reason)
		return
	}
	Skip(reason)
}
