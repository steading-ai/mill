package acceptance_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The graph half of the suite: a scratch dagger project, the fixture node
// filed in it, and the node read back with its result, its lease and its
// usage.
//
// It goes through `dg` rather than through mill's own dagger client on
// purpose. The client is what mill uses, so a suite that asserted through it
// would be asking mill to mark its own homework; `dg` is the operator's own
// tool and the one docs/prd.md §8 names in the done-when.

// daggerCLI is `dg` against one dagger, optionally as another principal.
type daggerCLI struct {
	socket  string
	project string
	config  string
}

// scratchGraph is a dagger project the suite made and will archive.
type scratchGraph struct {
	cli  daggerCLI
	Slug string
}

// newScratchGraph creates a dagger project for one scenario.
//
// Dagger has no delete: a project is archived, which makes it read-only and
// takes it out of every ready set, and that is what "cleaned up" means on
// this side (dagger's docs/domain-model.md §Project).
func newScratchGraph(ctx context.Context, name string) *scratchGraph {
	GinkgoHelper()
	slug := "mill-acceptance-" + name + "-" + nonce()
	cli := daggerCLI{socket: env.DaggerSocket}

	var created struct {
		Project struct {
			Slug string `json:"slug"`
		} `json:"project"`
	}
	cli.json(ctx, &created, "project", "create", slug, "--slug", slug)
	Expect(created.Project.Slug).To(Equal(slug))

	graph := &scratchGraph{cli: daggerCLI{socket: env.DaggerSocket, project: slug}, Slug: slug}
	DeferCleanup(func(ctx context.Context) {
		graph.cli.json(ctx, nil, "project", "archive", slug)
	})
	return graph
}

// file creates the fixture's node and returns its reference.
//
// `dg create` is a patch, and a patch answers with the nodes it touched:
// dagger's one way to mutate the graph is `patch`, and every verb that
// writes reports through it (dagger's docs/domain-model.md §Patch).
func (g *scratchGraph) file(ctx context.Context, title, body string) string {
	GinkgoHelper()
	var created struct {
		Nodes []struct {
			Reference string `json:"reference"`
		} `json:"nodes"`
	}
	g.cli.json(ctx, &created, "create", title, "--body", body, "--kind", "task", "--priority", "p1")
	Expect(created.Nodes).To(HaveLen(1), "dg create filed one node")
	Expect(created.Nodes[0].Reference).NotTo(BeEmpty())
	return created.Nodes[0].Reference
}

// nodeView is `dg node show` as the suite reads it.
type nodeView struct {
	Node struct {
		Reference string `json:"reference"`
		Status    string `json:"status"`
	} `json:"node"`
	Result *struct {
		Body      string `json:"body"`
		Artifacts []struct {
			Name  string `json:"name"`
			Parts []struct {
				Reference *struct {
					URI string `json:"uri"`
				} `json:"reference"`
				Text *struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"parts"`
		} `json:"artifacts"`
	} `json:"result"`
	// Artifacts are what the node carries, in attachment order. They are
	// not the result's: a run attaches a verdict or a plan while it is
	// still going, and a run that parked never wrote a result at all.
	Artifacts []nodeArtifact `json:"artifacts"`
	Lease     *struct {
		ID     string `json:"id"`
		Holder string `json:"holder"`
	} `json:"lease"`
	// Usage is dagger's rollup over every record reported on the node: by
	// model, with a count of the records each total sums.
	Usage struct {
		Actual []struct {
			Model        string `json:"model"`
			Records      int64  `json:"records,string"`
			InputTokens  int64  `json:"input_tokens,string"`
			OutputTokens int64  `json:"output_tokens,string"`
		} `json:"actual"`
	} `json:"usage"`
}

// show reads a node with its result, lease and usage.
func (g *scratchGraph) show(ctx context.Context, ref string) nodeView {
	GinkgoHelper()
	var view nodeView
	g.cli.json(ctx, &view, "node", "show", ref)
	return view
}

// nodeArtifact is one artifact as `dg node show` renders it.
type nodeArtifact struct {
	Name  string `json:"name"`
	Parts []struct {
		Text *struct {
			Text string `json:"text"`
		} `json:"text"`
	} `json:"parts"`
}

// text is an artifact's text content.
func (a nodeArtifact) text() string {
	for _, part := range a.Parts {
		if part.Text != nil {
			return part.Text.Text
		}
	}
	return ""
}

// attachment is the named artifact on the node itself — a verdict the run
// put there while it was still running — or a failure naming what the node
// does carry instead.
func (v nodeView) attachment(name string) string {
	GinkgoHelper()
	names := make([]string, 0, len(v.Artifacts))
	for _, a := range v.Artifacts {
		if a.Name == name {
			return a.text()
		}
		names = append(names, a.Name)
	}
	Fail("the node carries no " + name + " artifact; it carries: " + strings.Join(names, ", "))
	return ""
}

// gates are the gate nodes mill filed for a run, newest last, with their
// bodies. They are found by the labels mill puts on every gate it files
// (internal/dagger.GateLabels), which is how mill itself finds them again.
func (g *scratchGraph) gates(ctx context.Context, runID string) []string {
	GinkgoHelper()
	var listed struct {
		Nodes []struct {
			Reference string `json:"reference"`
			Title     string `json:"title"`
			Body      string `json:"body"`
		} `json:"nodes"`
	}
	g.cli.json(ctx, &listed, "node", "list", "--view", "full",
		"--label", "mill-gate", "--label", "mill-run-"+runID)
	bodies := make([]string, 0, len(listed.Nodes))
	for _, node := range listed.Nodes {
		bodies = append(bodies, node.Title+"\n\n"+node.Body)
	}
	return bodies
}

// artifact is the named artifact on a node's result, or a failure saying it
// is not there.
func (v nodeView) artifact(name string) string {
	GinkgoHelper()
	Expect(v.Result).NotTo(BeNil(), "the node carries a result")
	for _, a := range v.Result.Artifacts {
		if a.Name != name {
			continue
		}
		for _, part := range a.Parts {
			if part.Reference != nil {
				return part.Reference.URI
			}
			if part.Text != nil {
				return part.Text.Text
			}
		}
	}
	Fail("the result carries no " + name + " artifact")
	return ""
}

// preempt takes a live lease away from the worker holding it, which is how a
// scenario reaches the state a crash or a fence leaves behind without waiting
// out the TTL.
//
// The reason is `preempted` because that is the only one dagger accepts for
// this act: `expired` is the server's own, set by the expiry timer and
// refused from a client (dagger's docs/domain-model.md §Lease), and an
// operator taking a lease away is exactly what `preempted` means — mill's own
// `restart` and `cancel` release under it for the same reason
// (internal/run/restart.go). The holder cannot tell the routes apart: its
// next heartbeat is refused whichever one got there, and the next claim reads
// the ledger rather than the release reason (docs/ledger.md §Resume).
func (g *scratchGraph) preempt(ctx context.Context, leaseID, detail string) {
	GinkgoHelper()
	Expect(leaseID).NotTo(BeEmpty(), "there is a live lease to take")
	g.cli.json(ctx, nil, "release", leaseID, "--reason", "preempted", "--detail", detail)
}

// leaseID is the lease the node is held under, empty when it is held under
// none.
func (g *scratchGraph) leaseID(ctx context.Context, ref string) string {
	GinkgoHelper()
	view := g.show(ctx, ref)
	if view.Lease == nil {
		return ""
	}
	return view.Lease.ID
}

// claimAs takes the node under a second worker's lease, as the second
// principal when the environment named one. It returns the lease id.
func (g *scratchGraph) claimAs(ctx context.Context, agentRef string) string {
	GinkgoHelper()
	cli := g.cli
	cli.config = env.SecondDaggerConfig
	var claimed struct {
		Lease struct {
			ID string `json:"id"`
		} `json:"lease"`
	}
	cli.json(ctx, &claimed, "claim", "--harness", "acceptance", "--agent-ref", agentRef, "--ttl", "5m")
	Expect(claimed.Lease.ID).NotTo(BeEmpty(), "the second worker claimed the node")
	return claimed.Lease.ID
}

// handBack releases the second worker's lease so a fresh `mill run` can
// claim the node. The second worker never intended to do the work; taking
// the lease was the whole of its part.
func (g *scratchGraph) handBack(ctx context.Context, leaseID string) {
	GinkgoHelper()
	cli := g.cli
	cli.config = env.SecondDaggerConfig
	cli.json(ctx, nil, "release", leaseID,
		"--reason", "preempted", "--detail", "the acceptance suite handing the node back")
}

// json runs a `dg` command and decodes its JSON output into out, which may
// be nil for a command whose output the caller does not read.
func (c daggerCLI) json(ctx context.Context, out any, args ...string) {
	GinkgoHelper()
	full := []string{"--socket", c.socket, "--output", "json"}
	if c.project != "" {
		full = append(full, "--project", c.project)
	}
	if c.config != "" {
		full = append(full, "--config", c.config)
	}
	full = append(full, args...)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, env.DG, full...) //nolint:gosec // the suite's own dagger CLI
	command.Env = os.Environ()
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	Expect(err).NotTo(HaveOccurred(), "dg %s: %s", strings.Join(args, " "), stderr.String())
	if out == nil {
		return
	}
	Expect(json.Unmarshal([]byte(stdout.String()), out)).To(Succeed(),
		"dg %s: %s", strings.Join(args, " "), stdout.String())
}
