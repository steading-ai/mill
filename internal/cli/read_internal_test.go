package cli

import (
	"bytes"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// The read tools' rendering of the machine behind a run (docs/ledger.md
// §workspaces): what inspect and status print for a ledger row with a
// sandbox, and that a local run prints none of it. The responses are
// composed by hand, as a store would return them.

func inspectOf(workspace *millv1.Workspace) *millv1.InspectResponse {
	return &millv1.InspectResponse{
		Summary: &millv1.RunSummary{
			Run: &millv1.Run{
				Id: "01JRUN", NodeRef: "mill#80", Generation: 1, Branch: "mill/mill-80",
				Status: millv1.RunStatus_RUN_STATUS_DONE,
			},
			Workspace: workspace,
		},
	}
}

func machineWorkspace() *millv1.Workspace {
	four := int64(4)
	return &millv1.Workspace{
		Id: "01JWS", RunId: "01JRUN", Kind: millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE,
		State: millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED,
		Sandbox: &millv1.Sandbox{
			Machine: "mill-01jrun", Unit: "mill-machine@01jrun.service",
			ImageDigest: "sha256:3c9a43", Veth: "ve-mill-01jru",
			Lineages: []*millv1.SandboxLineage{
				{Scope: "global", Name: "mise", Restored: 3, Promoted: &four},
				{Scope: "repo/abc123", Name: "go-build", Restored: 0},
			},
		},
	}
}

func render(fn func(cmd *cobra.Command) error) string {
	GinkgoHelper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	Expect(fn(cmd)).To(Succeed())
	return out.String()
}

var _ = Describe("The read tools' view of the machine", Label("unit"), func() {
	// Scenario: Inspect shows it
	//   Given a finished machine-backed run
	//   When the operator runs mill inspect
	//   Then the machine, digest, veth and lineages are printed
	It("Inspect shows it", func() {
		By("given a finished machine-backed run")
		detail := inspectOf(machineWorkspace())

		By("when the operator runs mill inspect")
		out := render(func(cmd *cobra.Command) error { return renderInspect(cmd, detail, viewSummary) })

		By("then the machine, digest, veth and lineages are printed")
		Expect(out).To(ContainSubstring("machine   mill-01jrun (mill-machine@01jrun.service)"))
		Expect(out).To(ContainSubstring("image     sha256:3c9a43"))
		Expect(out).To(ContainSubstring("veth      ve-mill-01jru"))
		Expect(out).To(ContainSubstring("cache     global/mise restored 3 promoted 4"))
		Expect(out).To(ContainSubstring("cache     repo/abc123/go-build restored 0 promoted -"))
	})

	It("prints none of it for a local run", func() {
		local := &millv1.Workspace{Id: "01JWS", RunId: "01JRUN", Kind: millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL}
		for _, workspace := range []*millv1.Workspace{local, nil} {
			out := render(func(cmd *cobra.Command) error { return renderInspect(cmd, inspectOf(workspace), viewSummary) })
			Expect(out).To(ContainSubstring("branch    mill/mill-80"))
			Expect(out).NotTo(ContainSubstring("machine"))
			Expect(out).NotTo(ContainSubstring("image"))
			Expect(out).NotTo(ContainSubstring("veth"))
			Expect(out).NotTo(ContainSubstring("cache"))
		}
	})

	It("status names the machine, and the full view its digest, veth and caches", func() {
		runs := []*millv1.RunSummary{inspectOf(machineWorkspace()).GetSummary(), inspectOf(nil).GetSummary()}
		runs[1].Run.NodeRef = "mill#81"

		summary := render(func(cmd *cobra.Command) error { return renderStatus(cmd, runs, viewSummary) })
		Expect(summary).To(ContainSubstring("MACHINE"))
		Expect(rowOf(summary, "mill#80")).To(ContainSubstring("mill-01jrun"))
		local81 := strings.Fields(rowOf(summary, "mill#81"))
		Expect(local81[len(local81)-3]).To(Equal("-"), "a local run has no machine")
		Expect(summary).NotTo(ContainSubstring("sha256:3c9a43"))

		full := render(func(cmd *cobra.Command) error { return renderStatus(cmd, runs, viewFull) })
		row := rowOf(full, "mill#80")
		Expect(row).To(ContainSubstring("mill-01jrun"))
		Expect(row).To(ContainSubstring("sha256:3c9a43"))
		Expect(row).To(ContainSubstring("ve-mill-01jru"))
		Expect(row).To(ContainSubstring("global/mise@3->4, repo/abc123/go-build@0"))
		cells := strings.Fields(rowOf(full, "mill#81"))
		Expect(cells[len(cells)-6:]).To(Equal([]string{"-", "-", "-", "-", "-", "0"}),
			"a local run has none of the four machine cells, nor a last step, nor a revise round")
	})
})

var _ = Describe("mill status's held summary line", Label("unit"), func() {
	// Scenario: the line counts held apart from parked or unowned
	//   Given a listing mixing runs a worker holds with ones it does not
	//   When mill status renders the summary line
	//   Then it counts the two apart, not just the row total
	It("counts held apart from parked or unowned", func() {
		By("given a listing mixing runs a worker holds with ones it does not")
		runs := []*millv1.RunSummary{
			{Run: &millv1.Run{Id: "01JHELD1", NodeRef: "mill#80"}, Held: true},
			{Run: &millv1.Run{Id: "01JHELD2", NodeRef: "mill#81"}, Held: true},
			{Run: &millv1.Run{Id: "01JPARK1", NodeRef: "mill#82"}, Held: false},
		}

		By("when mill status renders the summary line")
		line := heldSummaryLine(runs)

		By("then it counts the two apart, not just the row total")
		Expect(line).To(Equal("2 held, 1 parked or unowned"))
	})

	It("reads 0 held for an empty listing", func() {
		Expect(heldSummaryLine(nil)).To(Equal("0 held, 0 parked or unowned"))
	})
})

// rowOf is the line of a table that starts with the given cell.
func rowOf(table, first string) string {
	for _, line := range bytes.Split([]byte(table), []byte("\n")) {
		if bytes.HasPrefix(line, []byte(first+" ")) || bytes.HasPrefix(line, []byte(first+"\t")) {
			return string(line)
		}
	}
	return ""
}
