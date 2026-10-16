// Pack → service input mappers for the approval and inbound-hook passes, shared by install and by
// the upgrade adapters (specs/069 L5, T092) so both read a pack item exactly the same way.
package vertical_pack

import (
	"strings"

	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/services"
)

// toInboundHookInput maps a pack hook to the service input. No secret anywhere: the service mints
// one on Create and leaves it alone on Update.
func toInboundHookInput(ph PackInboundHook) services.InboundHookInput {
	mapping := make([]inboundhook.Mapping, 0, len(ph.Mapping))
	for _, m := range ph.Mapping {
		mapping = append(mapping, inboundhook.Mapping{Source: m.Source, Field: m.Field})
	}
	in := services.InboundHookInput{
		Name:       strings.TrimSpace(ph.Name),
		EntityName: strings.TrimSpace(ph.EntityName),
		Mapping:    mapping,
		MatchField: strings.TrimSpace(ph.MatchField),
		IsActive:   ph.IsActive,
	}
	if pm := ph.PaymentMatch; pm != nil {
		in.PaymentMatch = &inboundhook.PaymentMatch{
			TargetEntity: strings.TrimSpace(pm.TargetEntity),
			RefField:     strings.TrimSpace(pm.RefField),
			RefPattern:   pm.RefPattern,
			MemoSource:   pm.MemoSource,
			StatusField:  strings.TrimSpace(pm.StatusField),
			StatusValue:  pm.StatusValue,
			AmountSource: pm.AmountSource,
			AmountField:  strings.TrimSpace(pm.AmountField),
		}
	}
	return in
}

// approvalUserApprover marks an approval whose step names a person: not portable, so the process
// is skipped (install) or refused (upgrade) instead of saved with nobody able to approve.
const approvalUserApprover = "__user__"

// approvalRefs resolve a role / team / queue NAME to the target tenant's id ("" = nowhere).
type approvalRefs struct{ role, team, queue func(string) string }

// toApprovalInput maps a pack approval to the service input, resolving every name through refs.
// unresolved != "" means the process cannot be built: the offending name (or approvalUserApprover),
// with kind = what that name was meant to be, for the message.
func toApprovalInput(pa PackApproval, userID string, refs approvalRefs) (in services.CreateApprovalProcessInput, unresolved, kind string) {
	steps := make([]services.ApprovalStepInput, 0, len(pa.Steps))
	for _, st := range pa.Steps {
		if a := st.Assignment; a != nil { // S6b: a role / team / queue BY NAME → the target tenant's id, or a field / submitter rule
			sin := services.ApprovalStepInput{ID: st.ID, Name: st.Name, SLASeconds: st.SLASeconds, SLAClock: st.SLAClock}
			ai := &services.ApprovalAssignmentInput{
				Type: strings.TrimSpace(a.Type), Field: strings.TrimSpace(a.Field), Strategy: strings.TrimSpace(a.Strategy),
			}
			ref := strings.TrimSpace(a.Ref)
			switch ai.Type {
			case "user": // a person is not portable
				return in, approvalUserApprover, "role"
			case "role":
				if ai.ID = refs.role(ref); ai.ID == "" {
					return in, ref, "role"
				}
			case "team":
				if ai.ID = refs.team(ref); ai.ID == "" {
					return in, ref, "nhóm"
				}
			case "queue":
				if ai.ID = refs.queue(ref); ai.ID == "" {
					return in, ref, "hàng đợi"
				}
			}
			sin.Assignment = ai
			if c := strings.TrimSpace(st.Completion); c != "" {
				sin.Completion = &services.ApprovalCompletionInput{Mode: c, Percent: st.CompletionPercent}
			}
			sin.OnReject = packOnRejectInput(st.OnReject)
			steps = append(steps, sin)
			continue
		}
		if len(st.Approvers) > 0 { // a parallel step (S4): every approver is a role NAME
			sin := services.ApprovalStepInput{ID: st.ID, Name: st.Name, SLASeconds: st.SLASeconds, SLAClock: st.SLAClock}
			for _, ap := range st.Approvers {
				if strings.TrimSpace(ap.Type) != "role" {
					return in, approvalUserApprover, "role"
				}
				roleID := refs.role(ap.Ref)
				if roleID == "" {
					return in, strings.TrimSpace(ap.Ref), "role"
				}
				sin.Approvers = append(sin.Approvers, services.ApprovalApproverInput{Type: "role", ID: roleID})
			}
			if c := strings.TrimSpace(st.Completion); c != "" {
				sin.Completion = &services.ApprovalCompletionInput{Mode: c, Percent: st.CompletionPercent}
			}
			sin.OnReject = packOnRejectInput(st.OnReject)
			steps = append(steps, sin)
			continue
		}
		if strings.TrimSpace(st.ApproverType) != "role" {
			return in, approvalUserApprover, "role"
		}
		ref := strings.TrimSpace(st.ApproverRef)
		roleID := refs.role(ref)
		if roleID == "" {
			return in, ref, "role"
		}
		steps = append(steps, services.ApprovalStepInput{
			ID:           st.ID,
			Name:         st.Name,
			ApproverType: st.ApproverType,
			ApproverID:   roleID,
			SLASeconds:   st.SLASeconds,
			SLAClock:     st.SLAClock,
			OnReject:     packOnRejectInput(st.OnReject),
		})
	}

	in = services.CreateApprovalProcessInput{
		Name:       pa.Name,
		EntityName: pa.EntityName,
		Trigger:    pa.Trigger,
		Steps:      steps,
		IsActive:   pa.IsActive,
		ActorID:    userID,
	}
	if pa.Outcome != nil {
		in.Outcome = &services.ApprovalOutcomeInput{
			StatusField:   pa.Outcome.StatusField,
			ApprovedValue: pa.Outcome.ApprovedValue,
			RejectedValue: pa.Outcome.RejectedValue,
			ReturnedValue: pa.Outcome.ReturnedValue,
		}
	}
	if pa.Criteria != nil {
		in.Match = pa.Criteria.Match
		for _, r := range pa.Criteria.Rules {
			in.Rules = append(in.Rules, services.ApprovalCriteriaRuleInput{Field: r.Field, Op: r.Op, Value: r.Value})
		}
	}
	return in, "", "role"
}
