// Package acceptance_test is the suite that drives the whole mill against
// real dependencies: a real skift, a real GitLab, the real sidecar, and a
// real Claude account.
//
// Every other suite in this tree proves a part. This one proves O0's
// done-when: a quick-fix node delivered by `mill run`, with every step's
// evidence in the ledger (docs/prd.md §8). Nothing here is faked, which is
// the point — skift's first build passed its entire suite while agents
// could not file work, and the lesson was that an acceptance suite must
// drive the system as the principal the design describes.
//
// It costs real money and real merge requests, so it is off unless
// MILL_ACCEPTANCE says otherwise. When it is on and something it needs is
// missing, it refuses before it spends anything and names everything that is
// missing at once; see docs/runbook.md.
//
// Adding a scenario is adding a fixture: a repository seed, a node body, and
// the expectation. See fixtures_test.go.
package acceptance_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAcceptance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "acceptance")
}
