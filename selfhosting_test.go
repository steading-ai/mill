package mill_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
)

// The self-hosting proof (docs/sandbox.md §Self-hosting, step 3): the
// deployed worker — `mill.service` on `core`, `workspace: machine`, mill in
// `nested_repos` — claimed a node filed in the mill project and delivered it
// inside a machine, and the host shows nothing of the run afterwards. The
// facts live in the deployed store's ledger and on the host, so the specs
// read those and nothing else; they are off unless MILL_SELF_HOSTING_PROOF
// names the node the deployed worker delivered, because on any other host
// there is no deployed worker to ask.
//
// The variables:
//
//   - MILL_SELF_HOSTING_PROOF — the proof node's reference (`mill#89`).
//   - MILL_SOCKET — the deployed store's socket; the unit's default when
//     unset.
//   - MILL_SANDBOX_ROOT — the deployed sandbox root; `/var/lib/mill` when
//     unset.
const (
	// liveTierStep is the check step the mill project's workflows add: the
	// live tier, run inside the machine on the committed tree
	// (workflows/mill-quick-fix, docs/sandbox.md §Self-hosting step 3).
	liveTierStep = "live-tier"

	selfHostingProofVar = "MILL_SELF_HOSTING_PROOF"
	selfHostingSkip     = "set MILL_SELF_HOSTING_PROOF=mill#<n> on the host whose deployed worker delivered the proof node to run the spec that reads its ledger"
)

// deployedStore dials the deployed service over its unix socket.
func deployedStore() millv1connect.StoreServiceClient {
	socket := os.Getenv("MILL_SOCKET")
	if socket == "" {
		socket = fmt.Sprintf("/run/user/%d/mill/mill.sock", os.Getuid())
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	return millv1connect.NewStoreServiceClient(client, "http://mill")
}

// deployedSandboxRoot is where the deployed worker keeps its lineages.
func deployedSandboxRoot() string {
	if root := os.Getenv("MILL_SANDBOX_ROOT"); root != "" {
		return root
	}
	return "/var/lib/mill"
}

// generationsOf lists a lineage's generations on the volume, by name.
func generationsOf(lineage string) []string {
	entries, err := os.ReadDir(lineage)
	Expect(err).NotTo(HaveOccurred(), lineage)
	var gens []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "gen-") {
			gens = append(gens, entry.Name())
		}
	}
	return gens
}

var _ = Describe("The self-hosting proof", Label("live"), func() {
	var proof string

	BeforeEach(func() {
		proof = os.Getenv(selfHostingProofVar)
		if proof == "" {
			Skip(selfHostingSkip)
		}
	})

	// Scenario: The deployed worker delivers inside a machine
	//   Given mill.service running with workspace machine and mill in nested_repos
	//   When a small node is filed in the mill project
	//   Then the worker delivers it and the ledger shows the machine, the
	//   digest, and the live tier's exit 0
	It("The deployed worker delivers inside a machine", func(ctx SpecContext) {
		By("given the deployed worker's ledger, and the node it was asked to deliver")
		res, err := deployedStore().Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{NodeRef: proof}))
		Expect(err).NotTo(HaveOccurred())
		detail := res.Msg

		By("then the run is done, and its workspace was a machine")
		Expect(detail.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		workspace := detail.GetSummary().GetWorkspace()
		Expect(workspace.GetKind()).To(Equal(millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE))
		sandbox := workspace.GetSandbox()
		Expect(sandbox.GetMachine()).To(HavePrefix("mill-"), "the ledger names the machine")
		Expect(sandbox.GetImageDigest()).To(HaveLen(64), "the ledger names the image digest")
		Expect(sandbox.GetLineages()).NotTo(BeEmpty(), "the ledger names the lineages")
		for _, lineage := range sandbox.GetLineages() {
			Expect(lineage.GetName()).NotTo(BeEmpty())
			Expect(lineage.GetPromoted()).To(BeNumerically(">=", lineage.GetRestored()),
				"%s/%s was promoted at the end", lineage.GetScope(), lineage.GetName())
		}

		By("and every step's last attempt closed on evidence; an earlier attempt a revise superseded is the ledger's story, not a step left open")
		latest := map[string]*millv1.StepRunDetail{}
		for _, step := range detail.GetSteps() {
			name := step.GetStepRun().GetStepName()
			if previous, ok := latest[name]; !ok || step.GetStepRun().GetAttempt() > previous.GetStepRun().GetAttempt() {
				latest[name] = step
			}
		}
		for name, step := range latest {
			Expect(step.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED), name)
		}

		By("and the live tier ran inside the machine as a check step, closing on exit 0")
		liveTier := latest[liveTierStep]
		Expect(liveTier).NotTo(BeNil(), "the run has no %s step: the mill project's workflow carries the live tier as a check step (workflows/mill-quick-fix)", liveTierStep)
		Expect(liveTier.GetStepRun().GetKind()).To(Equal(millv1.StepKind_STEP_KIND_CHECK))
		Expect(liveTier.GetEvidence().GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
		Expect(liveTier.GetEvidence().GetExit().GetStatus()).To(BeZero(), "the live tier's exit inside the machine")
	})

	// Scenario: Nothing outlives the run
	//   Given the proof node delivered
	//   When the host lists machines and mill's caches
	//   Then no machine is listed and each lineage has one generation
	It("Nothing outlives the run", func(ctx SpecContext) {
		By("given the proof node delivered, and the lineages its run promoted")
		res, err := deployedStore().Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{NodeRef: proof}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		sandbox := res.Msg.GetSummary().GetWorkspace().GetSandbox()

		By("when the host lists its machines")
		out, err := exec.CommandContext(ctx, "machinectl", "list", "--no-legend").CombinedOutput() //nolint:gosec // the host's own tool
		Expect(err).NotTo(HaveOccurred(), string(out))

		By("then no machine is listed")
		Expect(strings.TrimSpace(string(out))).To(BeEmpty(), "machinectl list")

		By("and each of the repository's lineages holds the one generation this run promoted, beside at most the one before it")
		// A promotion prunes a lineage to its two newest generations
		// (docs/sandbox.md §Promote): the first delivery of a repository
		// leaves one generation, and every later one leaves the run's own
		// beside its predecessor's. Either way the run's is the newest and
		// nothing older than the one before it stands.
		var repoLineages int
		for _, lineage := range sandbox.GetLineages() {
			if lineage.GetScope() == "global" {
				// Global lineages are shared across every repository.
				continue
			}
			repoLineages++
			dir := filepath.Join(deployedSandboxRoot(), "cache", lineage.GetScope(), lineage.GetName())
			generations := generationsOf(dir)
			Expect(generations).To(ContainElement(fmt.Sprintf("gen-%d", lineage.GetPromoted())),
				"%s/%s holds the generation this run promoted", lineage.GetScope(), lineage.GetName())
			Expect(len(generations)).To(BeNumerically("<=", 2), "%s/%s: %v", lineage.GetScope(), lineage.GetName(), generations)
			for _, generation := range generations {
				Expect(generation).To(BeElementOf(fmt.Sprintf("gen-%d", lineage.GetPromoted()), fmt.Sprintf("gen-%d", lineage.GetPromoted()-1)),
					"%s/%s: nothing older than the previous run's stands", lineage.GetScope(), lineage.GetName())
			}
		}
		Expect(repoLineages).To(BeNumerically(">=", 1), "the repository has lineages of its own")
	})
})
