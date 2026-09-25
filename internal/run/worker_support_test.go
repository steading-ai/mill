package run_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/skift"
)

// What the worker's specs need on top of the run loop's own fixture: nodes
// the claim filters can tell apart, a worker over the fixture's runner, and
// a way to read a run that is not the fixture's last node.

// workerOver builds the claim loop over one runner, which is one project.
func (f *fixture) workerOver(runner *run.Runner, options ...func(*run.WorkerOptions)) *run.Worker {
	GinkgoHelper()
	opts := run.WorkerOptions{
		Runner: runner,
		// Short enough that a spec waiting on an idle worker is not waiting
		// on a minute, long enough that the wait is a real one.
		Wait: 3 * time.Second,
		Out:  GinkgoWriter,
		Log:  slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
	}
	for _, option := range options {
		option(&opts)
	}
	worker, err := run.NewWorker(opts)
	Expect(err).NotTo(HaveOccurred())
	return worker
}

// start runs the worker in the background and reports what it returned.
func startWorker(ctx context.Context, worker *run.Worker) chan error {
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	return done
}

// labelledNode files an agent node carrying labels, which is what a claim
// filter selects on.
func (f *fixture) labelledNode(ctx context.Context, title, body string, labels ...string) string {
	GinkgoHelper()
	return f.createNode(ctx, &skiftv1.CreateNodeOp{Title: title, Body: body, Labels: labels})
}

// userNode files a node only a person may complete. To a worker it is
// invisible: `assignee_kind: agent` is not one of the claim's filters but
// the seam's own refusal, so a project holding nothing else is a project
// with no ready work.
func (f *fixture) userNode(ctx context.Context, title, body string) string {
	GinkgoHelper()
	return f.createNode(ctx, &skiftv1.CreateNodeOp{
		Title: title, Body: body, AssigneeKind: skiftv1.AssigneeKind_ASSIGNEE_KIND_USER,
	})
}

// createNode files one node and remembers it as the fixture's, so the
// helpers that read by reference read this one.
func (f *fixture) createNode(ctx context.Context, op *skiftv1.CreateNodeOp) string {
	GinkgoHelper()
	res, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-node-" + op.GetTitle(),
		Project:   project,
		Ops:       []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CreateNode{CreateNode: op}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	f.nodeRef = res.Msg.GetNodes()[0].GetReference()
	f.nodeTitle = op.GetTitle()
	return f.nodeRef
}

// complete finishes a node as a person does: no lease, no mill request id.
// It is how a spec makes a blocked node ready.
func (f *fixture) complete(ctx context.Context, ref string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-complete-" + ref, Project: project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
			Node: ref, Result: &skiftv1.Result{Body: "the spec finished it"},
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// statusOf is what skift says of one node.
func (f *fixture) statusOf(ctx context.Context, ref string) skiftv1.Status {
	GinkgoHelper()
	node, err := f.client.Node(ctx, ref)
	Expect(err).NotTo(HaveOccurred())
	return node.GetStatus()
}

// holdReasonOf is the reason skift holds a node under, or "" while it is
// not held.
func (f *fixture) holdReasonOf(ctx context.Context, ref string) string {
	GinkgoHelper()
	node, err := f.client.Node(ctx, ref)
	Expect(err).NotTo(HaveOccurred())
	return node.GetHold().GetReason()
}

// runOfRef is the ledger's record of a named node's run, or nil.
func (f *fixture) runOfRef(ctx context.Context, ref string) *millv1.Run {
	GinkgoHelper()
	node, err := f.client.Node(ctx, ref)
	Expect(err).NotTo(HaveOccurred())
	res, err := f.store.GetRunByNode(ctx, connect.NewRequest(&millv1.GetRunByNodeRequest{NodeId: node.GetId()}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetRun()
}

// counting records which skift procedures were called, so a spec can assert
// what the worker did *not* do — subscribe to a change stream — as well as
// what it did.
type counting struct {
	socket string

	claims atomic.Int64
	events atomic.Int64
}

func (c *counting) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/ClaimNext"):
		c.claims.Add(1)
	case strings.Contains(req.URL.Path, "EventService"):
		c.events.Add(1)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
		},
	}
	return transport.RoundTrip(req)
}

// counted builds a skift client whose calls this spec can count.
func (f *fixture) counted() (*skift.Client, *counting) {
	counter := &counting{socket: f.skift.Socket}
	return skift.New(&http.Client{Transport: counter}, f.skift.BaseURL(), project,
		skift.WithAgentRef(f.worker)), counter
}

// runningStep is the step whose attempt is in flight, and "" while the
// worker has not opened a run yet. A spec waiting for a step to begin has
// to tolerate the window before the ledger has anything to say.
func (f *fixture) runningStep(ctx context.Context, ref string) string {
	GinkgoHelper()
	if f.runOfRef(ctx, ref) == nil {
		return ""
	}
	return f.openStepRun(ctx).GetStepName()
}
