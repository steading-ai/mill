package server_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/server"
	"github.com/ivy/mill/internal/server/interceptor"
)

var _ = Describe("What a procedure needs", Label("unit"), func() {
	required := server.Requirement()

	DescribeTable("the read verbs need only read",
		func(procedure string) {
			Expect(required(procedure)).To(Equal(interceptor.PermissionRead))
		},
		Entry("GetRunByNode", millv1connect.StoreServiceGetRunByNodeProcedure),
		Entry("GetWorkflow", millv1connect.StoreServiceGetWorkflowProcedure),
		Entry("ListWorkflows", millv1connect.StoreServiceListWorkflowsProcedure),
		Entry("GetSkill", millv1connect.StoreServiceGetSkillProcedure),
		Entry("Status", millv1connect.StoreServiceStatusProcedure),
		Entry("ListRuns", millv1connect.StoreServiceListRunsProcedure),
		Entry("Inspect", millv1connect.StoreServiceInspectProcedure),
		Entry("WhoAmI", millv1connect.StoreServiceWhoAmIProcedure),
		Entry("Logs", millv1connect.StoreServiceLogsProcedure),
		Entry("WorkerService.Probe", millv1connect.WorkerServiceProbeProcedure),
	)

	DescribeTable("the verbs a human or an agent drives need operate",
		func(procedure string) {
			Expect(required(procedure)).To(Equal(interceptor.PermissionOperate))
		},
		Entry("Cancel", millv1connect.StoreServiceCancelProcedure),
		Entry("Rerun", millv1connect.StoreServiceRerunProcedure),
		Entry("Abandon", millv1connect.StoreServiceAbandonProcedure),
		Entry("PutWorkflow", millv1connect.StoreServicePutWorkflowProcedure),
		Entry("PutSkill", millv1connect.StoreServicePutSkillProcedure),
	)

	DescribeTable("the calls that write a run's record need write",
		func(procedure string) {
			Expect(required(procedure)).To(Equal(interceptor.PermissionWrite))
		},
		Entry("CreateRun", millv1connect.StoreServiceCreateRunProcedure),
		Entry("SetLease", millv1connect.StoreServiceSetLeaseProcedure),
		Entry("StartStepRun", millv1connect.StoreServiceStartStepRunProcedure),
		Entry("CloseStepRun", millv1connect.StoreServiceCloseStepRunProcedure),
		Entry("FailStepRun", millv1connect.StoreServiceFailStepRunProcedure),
		Entry("AbortOpenStepRuns", millv1connect.StoreServiceAbortOpenStepRunsProcedure),
		Entry("SupersedeSteps", millv1connect.StoreServiceSupersedeStepsProcedure),
		Entry("MarkWaiting", millv1connect.StoreServiceMarkWaitingProcedure),
		Entry("Park", millv1connect.StoreServiceParkProcedure),
		Entry("Done", millv1connect.StoreServiceDoneProcedure),
		Entry("IssueStepToken", millv1connect.StoreServiceIssueStepTokenProcedure),
		Entry("RevokeStepToken", millv1connect.StoreServiceRevokeStepTokenProcedure),
		Entry("RecordUsage", millv1connect.StoreServiceRecordUsageProcedure),
		Entry("MarkUsagePosted", millv1connect.StoreServiceMarkUsagePostedProcedure),
	)

	It("an RPC nobody classified is reachable only by the service's own uid", func() {
		Expect(required("/mill.v1.StoreService/SomethingNew")).To(Equal(interceptor.PermissionWrite))
	})
})
