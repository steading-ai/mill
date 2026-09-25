package acceptance_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The skift the suite files into. Every other scenario here is about what
// mill does; these two are about what the suite does, which is to touch
// nothing it does not own (docs/core-principles.md). Skift has no delete,
// so a scratch project made in the operator's graph is there until someone
// archives it, and a suite killed before its cleanup would leave one. The
// answer is to file nowhere anyone reads: the suite's own skift, from
// internal/testsupport, in a container the reaper takes with it.

// deployedSkift is the operator's skift, if this host has one the suite
// can see: the one `skift` in the operator's shell would reach. The suite
// never files there; these scenarios read its project list to prove it.
// Empty when there is none, in which case there is nothing to protect.
func deployedSkift() string {
	candidates := []string{os.Getenv("SKIFT_SOCKET")}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "skift", "skift.sock"))
	}
	candidates = append(candidates, deployedSkiftSocket)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// projectSlugs is every project a skift lists, or the reason it could not
// be asked. A deployed skift this account cannot read is one it cannot
// write either, so a refusal is reported rather than failed on.
func projectSlugs(ctx context.Context, socket string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, env.Skift, "--socket", socket, "--output", "json", "project", "list") //nolint:gosec // the suite's own skift CLI
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, &listError{socket: socket, detail: stderr.String(), err: err}
	}
	var listed struct {
		Projects []struct {
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(listed.Projects))
	for _, p := range listed.Projects {
		slugs = append(slugs, p.Slug)
	}
	return slugs, nil
}

type listError struct {
	socket, detail string
	err            error
}

func (e *listError) Error() string {
	return "skift --socket " + e.socket + " project list: " + e.err.Error() + ": " + e.detail
}

// ownSkift skips the calling scenario when the environment pointed the
// suite at an external skift. Both scenarios below are about the default,
// and against an external skift there is nothing to prove: filing there
// was the operator's decision.
func ownSkift() {
	GinkgoHelper()
	if env.ExternalSkift {
		Skip(SkiftSocketVar + " is set, so the suite files in the skift it was given rather than one of its own")
	}
}

// deployedProjects is the deployed skift's project list, and where it was
// read from. It is empty with an entry in the report when there is no
// deployed skift to read, or none this account may.
func deployedProjects(ctx context.Context) (socket string, slugs []string) {
	GinkgoHelper()
	socket = deployedSkift()
	if socket == "" {
		AddReportEntry("the deployed skift", "none on this host; nothing to protect")
		return "", nil
	}
	slugs, err := projectSlugs(ctx, socket)
	if err != nil {
		AddReportEntry("the deployed skift", socket+" could not be read: "+err.Error())
		return "", nil
	}
	return socket, slugs
}

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: The suite owns the skift it files into
	//   Given no skift socket is named in the environment
	//   When the acceptance suite runs
	//   Then it starts a skift of its own and files its scratch projects there
	//   And the deployed skift's project list is unchanged
	It("The suite owns the skift it files into", func(ctx SpecContext) {
		By("given no skift socket is named in the environment")
		ownSkift()
		Expect(os.Getenv(SkiftSocketVar)).To(BeEmpty())
		deployed, before := deployedProjects(ctx)

		By("when the acceptance suite runs")
		// It is running: the BeforeSuite started the skift every scenario
		// files into, and this scenario files as every other one does.
		graph := newScratchGraph(ctx, "owns", placeholderVCSURL)

		By("then it starts a skift of its own and files its scratch projects there")
		Expect(env.SkiftSocket).NotTo(BeEmpty(), "the BeforeSuite started a skift")
		Expect(env.SkiftSocket).NotTo(Equal(deployed), "the suite's skift is not the deployed one")
		Expect(env.SkiftSocket).To(HavePrefix(os.TempDir()+string(os.PathSeparator)+"mill-skift-"),
			"the suite's skift listens on a socket in a directory the suite owns")
		own, err := projectSlugs(ctx, env.SkiftSocket)
		Expect(err).NotTo(HaveOccurred())
		Expect(own).To(ContainElement(graph.Slug), "the scratch project is in the suite's own skift")

		By("and the deployed skift's project list is unchanged")
		if deployed == "" {
			return
		}
		after, err := projectSlugs(ctx, deployed)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(ConsistOf(before), "filing a scratch project changed nothing in %s", deployed)
		Expect(after).NotTo(ContainElement(graph.Slug))
	})

	// Scenario: A killed suite leaves the operator's graph alone
	//   Given the acceptance suite is running against its own skift
	//   When it is killed before its cleanup runs
	//   Then the deployed skift holds no scratch project
	It("A killed suite leaves the operator's graph alone", func(ctx SpecContext) {
		By("given the acceptance suite is running against its own skift")
		ownSkift()
		Expect(env.SkiftSocket).NotTo(BeEmpty(), "the BeforeSuite started a skift")
		deployed, before := deployedProjects(ctx)

		By("when it is killed before its cleanup runs")
		// What a kill leaves is a scratch project whose archive never ran.
		// The scenario makes exactly that — a project with no cleanup
		// registered, as newScratchGraph would have made it — rather than
		// killing a second suite, because the question is where the
		// leftover lives, and that is decided when the project is created
		// and not when the process dies.
		slug := "mill-acceptance-killed-" + nonce()
		cli := skiftCLI{socket: env.SkiftSocket}
		var created struct {
			Project struct {
				Slug string `json:"slug"`
			} `json:"project"`
		}
		cli.json(ctx, &created, "project", "create", slug, "--slug", slug)
		Expect(created.Project.Slug).To(Equal(slug))
		own, err := projectSlugs(ctx, env.SkiftSocket)
		Expect(err).NotTo(HaveOccurred())
		Expect(own).To(ContainElement(slug), "the leftover is in the suite's own skift, which goes with the suite")

		By("then the deployed skift holds no scratch project")
		if deployed == "" {
			return
		}
		after, err := projectSlugs(ctx, deployed)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(ContainElement(slug))
		Expect(after).To(ConsistOf(before), "the leftover changed nothing in %s", deployed)
	})
})
