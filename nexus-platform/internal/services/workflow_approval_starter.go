package services

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"nexus/platform/internal/bpm"
)

// approvalSender is the slice of *bpm.Requester the start_approval action needs.
type approvalSender interface {
	SubmitWith(ctx context.Context, tenantID, entityName, recordID string, data map[string]any, actor bpm.Actor, opt bpm.SubmitOptions) (*bpm.Instance, error)
}

// WorkflowApprovalStarter satisfies workflow.ApprovalStarter over the approval engine's manual send
// (specs/043 S9b): a rule starts a hồ sơ through exactly the path "Gửi duyệt" takes — criteria on the
// record, one open approval per record — acting as the record's owner.
//
// It is bound AFTER construction (Use): the workflow scheduler snapshots its action dependencies
// when it is built, long before the approval requester exists (the requester needs the record
// service, which needs the workflow engine). The binding is atomic because that scheduler already
// runs on its own goroutine by then.
type WorkflowApprovalStarter struct {
	sender atomic.Pointer[approvalSender]
}

// NewWorkflowApprovalStarter returns an unbound starter; until Use is called every start is an error.
func NewWorkflowApprovalStarter() *WorkflowApprovalStarter { return &WorkflowApprovalStarter{} }

// Use binds the approval engine.
func (w *WorkflowApprovalStarter) Use(s approvalSender) { w.sender.Store(&s) }

// StartApproval sends the record through processID as submitterID. started=false with a nil error
// means the record already waits on an approval (the open-key guard said so).
func (w *WorkflowApprovalStarter) StartApproval(ctx context.Context, tenantID, entityName, recordID, processID, submitterID string, data map[string]any) (bool, error) {
	sp := w.sender.Load()
	if sp == nil {
		return false, fmt.Errorf("services.WorkflowApprovalStarter.StartApproval: approval engine not bound")
	}
	_, err := (*sp).SubmitWith(ctx, tenantID, entityName, recordID, data, bpm.Actor{UserID: submitterID}, bpm.SubmitOptions{ProcessID: processID})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, bpm.ErrAlreadyPending):
		return false, nil
	default:
		return false, fmt.Errorf("services.WorkflowApprovalStarter.StartApproval: %w", err)
	}
}
