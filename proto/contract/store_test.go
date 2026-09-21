package contract_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// readOnlyRPCs are the calls that only read. Everything else on
// StoreService writes, and therefore has to be fenced by a lease.
var readOnlyRPCs = map[string]bool{
	"GetRunByNode":  true,
	"GetWorkflow":   true,
	"ListWorkflows": true,
	"GetSkill":      true,
	"Status":        true,
	"ListRuns":      true,
	"Inspect":       true,
	"History":       true,
	"Cost":          true,
	"Health":        true,
	"WhoAmI":        true,
	"Logs":          true,
	"WatchRuns":     true,
}

// leaselessRPCs are the mutations no lease fences, because their caller
// holds none. Cancel is a request a human or an agent makes to the run's
// worker: it sets a flag the worker reads at its next heartbeat and moves
// nothing itself, and the principal making it is the connection's own uid.
// Rerun is the same kind of act on a run no worker holds: the person says
// the work from a step is to be redone, and the ledger is what says so.
// RecordReconciliation is about no run at all: it is the record of what a
// worker found on its host before it claimed anything. SealAttempt is about
// an attempt that has already closed and released its own lease; activity
// is not one of the tables a lease fences.
var leaselessRPCs = map[string]bool{
	"Cancel":               true,
	"Rerun":                true,
	"RecordReconciliation": true,
	"SealAttempt":          true,
}

// wantedRPCs is exactly the surface docs/ledger.md's resume algorithm calls,
// plus the reads the CLI needs: status, listing ended runs, inspect, the
// skill a workflow pull writes back out, and the change stream mill status
// --follow reads. An RPC with no caller there does not belong on this
// service.
var wantedRPCs = []string{
	"GetRunByNode", "CreateRun", "SetLease",
	"StartStepRun", "RecordStepEnvironment", "MarkSessionStarted", "CloseStepRun", "FailStepRun",
	"AbortOpenStepRuns", "SupersedeSteps",
	"MarkWaiting", "Park", "RecordNotice", "RecordHeartbeat", "RecordInstall", "RecordReconciliation", "RecordWorkspace", "FinishWorkspace",
	"SetCompleteRequestId", "Done", "Fail", "Resume", "Abandon", "Cancel", "Rerun",
	"IssueStepToken", "RevokeStepToken",
	"RecordUsage", "MarkUsagePosted",
	"GetWorkflow", "ListWorkflows", "PutWorkflow", "PutSkill", "GetSkill",
	"Status", "ListRuns", "Inspect", "History", "Cost", "Health", "WhoAmI", "Logs",
	"SealAttempt", "WatchRuns",
}

var _ = Describe("store.proto", func() {
	var c contract

	BeforeEach(func() {
		c = parse(storeProto)
		Expect(c.rpcs).NotTo(BeEmpty())
	})

	It("declares exactly the RPCs the resume algorithm needs", func() {
		Expect(c.rpcNames()).To(ConsistOf(wantedRPCs))
		Expect(c.services).To(HaveKey("StoreService"))
	})

	It("carries a lease id on every mutating request", func() {
		for _, r := range c.rpcs {
			if readOnlyRPCs[r.name] || leaselessRPCs[r.name] {
				continue
			}
			f := c.field(r.request, "lease_id")
			Expect(f.typ).To(Equal("string"), "%s.lease_id", r.request)
			Expect(f.comment).NotTo(BeEmpty(), "%s.lease_id", r.request)
		}
	})

	It("says on a leaseless mutation why it carries none", func() {
		for name := range leaselessRPCs {
			_, ok := c.lookup(name+"Request", "lease_id")
			Expect(ok).To(BeFalse(), "%sRequest carries a lease it cannot be fenced by", name)
			// The explanation sits on the run the request names, or on the
			// request itself when it names no run.
			if runID, ok := c.lookup(name+"Request", "run_id"); ok {
				Expect(runID.comment).To(ContainSubstring("carries no lease"), name)
				continue
			}
			Expect(c.messages[name+"Request"]).To(ContainSubstring("no lease"), name)
		}
	})

	It("says the lease is compared inside the writing transaction", func() {
		comment := c.services["StoreService"]
		Expect(comment).To(ContainSubstring("lease_id"))
		Expect(comment).To(ContainSubstring("inside the same transaction as the write"))
		Expect(comment).To(ContainSubstring("rolls the transaction back on mismatch"))
	})

	It("names the exceptions to the fencing rule on their own requests", func() {
		// SetLease is the write that changes what later comparisons compare
		// against; PutWorkflow and PutSkill write rows that belong to no run;
		// FinishWorkspace is written after the run has ended and released
		// its lease. Each says so where an implementer will read it.
		Expect(c.field("SetLeaseRequest", "lease_id").comment).To(
			ContainSubstring("not compared against the stored one"))
		Expect(c.field("FinishWorkspaceRequest", "lease_id").comment).To(
			ContainSubstring("once the run has ended"))
		for _, m := range []string{"PutWorkflowRequest", "PutSkillRequest"} {
			Expect(c.field(m, "lease_id").comment).To(
				ContainSubstring("Empty for a CLI push"), m)
		}
	})

	It("pairs every RPC with its own request and response messages", func() {
		for _, r := range c.rpcs {
			Expect(r.request).To(Equal(r.name+"Request"), r.name)
			Expect(r.response).To(Equal(r.name+"Response"), r.name)
			Expect(r.comment).NotTo(BeEmpty(), r.name)
		}
	})

})
