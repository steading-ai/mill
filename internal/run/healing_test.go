package run_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/skift"
	"github.com/steading-ai/mill/internal/testsupport"
)

// docs/architecture.md §"Failure and stop": a red default branch files one
// bug node per repository, kept current as the red sha moves, and dropped
// or quietly marked green once the branch recovers. RedMainFindings is the
// probe; Reconcile is the seam a webhook receiver plugs its own findings
// into later without a poll loop of its own.

// countingTransport counts every ApplyPatch RPC attempt made through it —
// what proves Reconcile never even attempted a write it judged unnecessary,
// rather than merely proving skift replayed one as a no-op (its own
// request-id dedup cannot tell the two apart).
type countingTransport struct {
	*http.Transport
	mu    sync.Mutex
	count int
}

func newCountingTransport(socket string) *countingTransport {
	return &countingTransport{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func (c *countingTransport) applyPatchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/ApplyPatch") {
		c.mu.Lock()
		c.count++
		c.mu.Unlock()
	}
	return c.Transport.RoundTrip(req)
}

var _ = Describe("The healing reconciler", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context
	var counting *countingTransport
	var base *skift.Client

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		counting = newCountingTransport(f.skift.Socket)
		base = skift.New(&http.Client{Transport: counting}, f.skift.BaseURL(), project)
	})

	redMain := func(key, body string) run.Finding {
		return run.Finding{
			Key: key, Project: project,
			Title: "main is red at abc1234: build",
			Body:  body,
		}
	}

	// Scenario: The same finding arrives from two sources
	//   Given a red-main finding already reconciled
	//   When the same finding is reconciled again
	//   Then no node, comment or drop is written
	It("the same finding arrives from two sources", func() {
		red := redMain("red-main:group/thing:main", "group/thing's main is red at "+strings.Repeat("a", 40)+".")

		actions, err := run.Reconcile(ctx, base, []string{project}, []run.Finding{red})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(HaveLen(1))
		Expect(actions[0].Verb).To(Equal("filed"))
		Expect(counting.applyPatchCount()).To(Equal(1))

		found, err := base.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(HaveLen(1))
		Expect(found[0].GetKind()).To(Equal("bug"))
		Expect(found[0].GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT))
		Expect(found[0].GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P1))
		Expect(found[0].GetLabels()).To(ContainElements(skift.HealingLabel, skift.KeyLabel(red.Key)))

		before := counting.applyPatchCount()
		actions, err = run.Reconcile(ctx, base, []string{project}, []run.Finding{red})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(BeEmpty())
		Expect(counting.applyPatchCount()).To(Equal(before), "an unchanged finding attempts no write")
	})

	It("comments a moved sha on the node it already filed, and never repeats the same comment", func() {
		red := redMain("red-main:group/thing:main", "first: "+strings.Repeat("a", 40))
		_, err := run.Reconcile(ctx, base, []string{project}, []run.Finding{red})
		Expect(err).NotTo(HaveOccurred())

		moved := red
		moved.Body = "second: " + strings.Repeat("b", 40)
		before := counting.applyPatchCount()
		actions, err := run.Reconcile(ctx, base, []string{project}, []run.Finding{moved})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(HaveLen(1))
		Expect(actions[0].Verb).To(Equal("commented"))
		Expect(counting.applyPatchCount()).To(Equal(before + 1))

		found, err := base.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(HaveLen(1), "the moved sha comments the same node rather than filing a second one")

		// A further call with the identical, already-commented finding
		// attempts nothing more.
		before = counting.applyPatchCount()
		actions, err = run.Reconcile(ctx, base, []string{project}, []run.Finding{moved})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(BeEmpty())
		Expect(counting.applyPatchCount()).To(Equal(before))
	})

	// Scenario: Main heals itself before anyone claims the node
	//   Given an open red-main node nobody has claimed
	//   When main's head is green at the next sweep
	//   Then the node is dropped as no longer relevant
	It("main heals itself before anyone claims the node", func() {
		red := redMain("red-main:group/thing:main", "red: "+strings.Repeat("a", 40))
		filed, err := run.Reconcile(ctx, base, []string{project}, []run.Finding{red})
		Expect(err).NotTo(HaveOccurred())
		Expect(filed).To(HaveLen(1))

		actions, err := run.Reconcile(ctx, base, []string{project}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(HaveLen(1))
		Expect(actions[0].Verb).To(Equal("dropped"))

		found, err := base.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeEmpty(), "the dropped node no longer shows as an open finding")
	})

	It("comments-green a claimed finding instead of dropping it, and does not repeat once green", func() {
		red := redMain("red-main:group/other:main", "red: "+strings.Repeat("a", 40))
		filed, err := run.Reconcile(ctx, base, []string{project}, []run.Finding{red})
		Expect(err).NotTo(HaveOccurred())
		Expect(filed).To(HaveLen(1))

		_, err = base.Claim(ctx, filed[0].Ref)
		Expect(err).NotTo(HaveOccurred())

		actions, err := run.Reconcile(ctx, base, []string{project}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(HaveLen(1))
		Expect(actions[0].Verb).To(Equal("commented-green"))

		found, err := base.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(HaveLen(1), "a claimed node is commented, not dropped")
		comments, err := base.Comments(ctx, found[0].GetReference())
		Expect(err).NotTo(HaveOccurred())
		Expect(comments[len(comments)-1].GetBody()).To(Equal("main is green now."))

		before := counting.applyPatchCount()
		actions, err = run.Reconcile(ctx, base, []string{project}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(BeEmpty(), "a claimed, already-green node is left alone for good")
		Expect(counting.applyPatchCount()).To(Equal(before))
	})

	It("routes each project's finding into its own project, never the other's", func() {
		second := f.secondProject(ctx)
		mine := redMain("red-main:group/mine:main", "red: "+strings.Repeat("a", 40))
		theirs := run.Finding{
			Key: "red-main:group/theirs:main", Project: second,
			Title: "main is red at abc1234: build", Body: "red: " + strings.Repeat("b", 40),
		}

		actions, err := run.Reconcile(ctx, base, []string{project, second}, []run.Finding{mine, theirs})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions).To(HaveLen(2))

		mineFound, err := base.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(mineFound).To(HaveLen(1))

		theirsFound, err := base.Scoped(second).Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(theirsFound).To(HaveLen(1))
		Expect(theirsFound[0].GetReference()).NotTo(Equal(mineFound[0].GetReference()))
	})
})

var _ = Describe("The red-main probe", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	projectNamed := func(f *fixture) func(context.Context, string) (*skiftv1.Project, error) {
		return func(ctx context.Context, slug string) (*skiftv1.Project, error) {
			return f.client.ProjectNamed(ctx, slug)
		}
	}

	It("finds a repository whose default branch is red, naming the repository, ref, sha and failing checks", func() {
		f.forge.checksAt = forge.Failed
		f.forge.failingChecks = []forge.CheckResult{
			{Name: "build", URL: "https://forge.test/group/thing/builds/1"},
			{Name: "lint", URL: "https://forge.test/group/thing/builds/2"},
		}
		f.forge.defaultBranch = "main"
		f.forge.defaultSHA = strings.Repeat("c", 40)
		f.forge.credential = "forge-secret-token"

		var warnings []string
		findings := run.RedMainFindings(ctx, []string{project}, projectNamed(f), f.repositories(),
			func(p string, err error) { warnings = append(warnings, p+": "+err.Error()) })

		Expect(warnings).To(BeEmpty())
		Expect(findings).To(HaveLen(1))
		found := findings[0]
		Expect(found.Title).To(ContainSubstring(strings.Repeat("c", 7)))
		Expect(found.Title).To(ContainSubstring("build"))
		Expect(found.Body).To(ContainSubstring("main"))
		Expect(found.Body).To(ContainSubstring(strings.Repeat("c", 40)))
		Expect(found.Body).To(ContainSubstring("build"))
		Expect(found.Body).To(ContainSubstring("lint"))
		Expect(found.Body).To(ContainSubstring("https://forge.test/group/thing/builds/1"))
		Expect(found.Body).NotTo(ContainSubstring(f.worker), "the probe never names the worker's own host")
		Expect(found.Body).NotTo(ContainSubstring(f.forge.credential), "the probe never names the forge's credential")
	})

	It("finds nothing when the default branch is pending or green", func() {
		for _, status := range []forge.Status{forge.Pending, forge.Success} {
			f.forge.checksAt = status
			findings := run.RedMainFindings(ctx, []string{project}, projectNamed(f), f.repositories(),
				func(string, error) {})
			Expect(findings).To(BeEmpty(), "status %s should yield no finding", status)
		}
	})

	It("warns and continues past a project the probe cannot reach", func() {
		failing := func(ctx context.Context, slug string) (*skiftv1.Project, error) {
			if slug == "broken" {
				return nil, errors.New("no such project")
			}
			return f.client.ProjectNamed(ctx, slug)
		}
		f.forge.checksAt = forge.Failed

		var warnings []string
		findings := run.RedMainFindings(ctx, []string{"broken", project}, failing, f.repositories(),
			func(p string, err error) { warnings = append(warnings, p) })

		Expect(warnings).To(Equal([]string{"broken"}))
		Expect(findings).To(HaveLen(1), "the healthy project after the broken one still yields its finding")
	})
})

var _ = Describe("The sweep's healing pass", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	// Scenario: A catch-up race breaks main
	//   Given two branches merged within a minute and main's checks failed on the second
	//   When the sweep runs
	//   Then one bug node is open in the project naming the sha and the failing check
	//   And the next sweep files nothing more
	It("a catch-up race breaks main", func() {
		f.forge.checksAt = forge.Failed
		f.forge.defaultBranch = "main"
		f.forge.defaultSHA = strings.Repeat("c", 40)
		f.forge.failingChecks = []forge.CheckResult{
			{Name: "build", URL: "https://forge.test/group/thing/builds/1"},
		}

		By("when the sweep runs")
		report, err := f.runner().Sweep(ctx, nil, []string{project})
		Expect(err).NotTo(HaveOccurred())

		By("then one bug node is open in the project naming the sha and the failing check")
		Expect(report.Of(run.FindingHealFiled)).To(HaveLen(1))
		found, err := f.client.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(HaveLen(1))
		Expect(found[0].GetTitle()).To(ContainSubstring(strings.Repeat("c", 7)))
		Expect(found[0].GetTitle()).To(ContainSubstring("build"))
		Expect(found[0].GetBody()).To(ContainSubstring(strings.Repeat("c", 40)))
		Expect(found[0].GetBody()).To(ContainSubstring("build"))

		By("and the next sweep files nothing more")
		again, err := f.runner().Sweep(ctx, nil, []string{project})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Of(run.FindingHealFiled)).To(BeEmpty())
		Expect(again.Of(run.FindingHealCommented)).To(BeEmpty())
		stillFound, err := f.client.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(stillFound).To(HaveLen(1), "the second sweep filed nothing more")
	})
})

// repositories is the fixture's own Repositories resolver, exposed here
// exactly as the runner's own Options carries it, so the probe specs above
// call it the way Sweep does.
func (f *fixture) repositories() run.Repositories {
	return func(context.Context, *skiftv1.Project) (run.Repository, error) {
		return run.Repository{Forge: f.forge, Origin: f.origin}, nil
	}
}
