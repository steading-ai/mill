package server

import (
	millv1connect "github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/server/interceptor"
)

// permissions is how much authority each RPC needs, and the whole of what
// the roles in docs/architecture.md §"Identity and credentials" mean:
//
//   - read — the ledger seen and nothing changed. Every role has it.
//   - operate — cancel, rerun and restart, and the definitions a push
//     stores. The verbs a human or an agent drives; they ask the worker for
//     something and write no evidence.
//   - write — the record of a run. The worker's own calls, made by the
//     account the service runs as and by no other.
//
// A procedure absent from this table needs [interceptor.PermissionWrite]:
// an RPC added without a decision about who may call it is one only the
// service's own uid reaches.
var permissions = map[string]interceptor.Permission{
	millv1connect.StoreServiceGetRunByNodeProcedure:  interceptor.PermissionRead,
	millv1connect.StoreServiceGetWorkflowProcedure:   interceptor.PermissionRead,
	millv1connect.StoreServiceListWorkflowsProcedure: interceptor.PermissionRead,
	millv1connect.StoreServiceGetSkillProcedure:      interceptor.PermissionRead,
	millv1connect.StoreServiceStatusProcedure:        interceptor.PermissionRead,
	millv1connect.StoreServiceInspectProcedure:       interceptor.PermissionRead,
	millv1connect.StoreServiceHistoryProcedure:       interceptor.PermissionRead,
	millv1connect.StoreServiceCostProcedure:          interceptor.PermissionRead,
	millv1connect.StoreServiceHealthProcedure:        interceptor.PermissionRead,
	millv1connect.StoreServiceWhoAmIProcedure:        interceptor.PermissionRead,
	millv1connect.StoreServiceLogsProcedure:          interceptor.PermissionRead,
	millv1connect.WorkerServiceProbeProcedure:        interceptor.PermissionRead,

	millv1connect.StoreServiceCancelProcedure:      interceptor.PermissionOperate,
	millv1connect.StoreServiceRerunProcedure:       interceptor.PermissionOperate,
	millv1connect.StoreServiceAbandonProcedure:     interceptor.PermissionOperate,
	millv1connect.StoreServicePutWorkflowProcedure: interceptor.PermissionOperate,
	millv1connect.StoreServicePutSkillProcedure:    interceptor.PermissionOperate,
}

// Requirement is the table as the interceptor reads it.
func Requirement() interceptor.Requirement {
	return func(procedure string) interceptor.Permission {
		if permission, ok := permissions[procedure]; ok {
			return permission
		}
		return interceptor.PermissionWrite
	}
}
