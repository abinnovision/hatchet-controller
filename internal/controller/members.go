package controller

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
)

// tenantAccess is the members and pending invites of a tenant, indexed by lowercase email.
type tenantAccess struct {
	members map[string]hatchet.Member
	invites map[string][]hatchet.Invite
}

// listAccess reads the members and pending invites of the tenant.
func listAccess(ctx context.Context, hc hatchet.Client, tenantID string) (tenantAccess, error) {
	members, err := hc.ListMembers(ctx, tenantID)
	if err != nil {
		return tenantAccess{}, err
	}
	invites, err := hc.ListInvites(ctx, tenantID)
	if err != nil {
		return tenantAccess{}, err
	}
	out := tenantAccess{members: make(map[string]hatchet.Member, len(members)), invites: map[string][]hatchet.Invite{}}
	for _, m := range members {
		out.members[strings.ToLower(m.Email)] = m
	}
	for _, i := range invites {
		email := strings.ToLower(i.Email)
		out.invites[email] = append(out.invites[email], i)
	}
	return out, nil
}

// ensureMembers reconciles spec.members against Hatchet and removes the people the controller invited that left the spec.
// It records the people it manages in status and returns the first failure.
func (r *HatchetTenantReconciler) ensureMembers(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant) error {
	// Listing needs ADMIN, which an inherited tenant may not grant.
	if len(tenant.Spec.Members) == 0 && len(tenant.Status.Members) == 0 {
		return nil
	}
	tenantID := tenant.Status.TenantID
	var first error
	fail := func(label string, err error) {
		if first == nil {
			first = scopedError(label, hatchetv1alpha1.ReasonMemberFailed, hatchetError(err))
		}
	}

	access, err := listAccess(ctx, hc, tenantID)
	if err != nil {
		fail("members", err)
		return first
	}

	desired := make(map[string]bool, len(tenant.Spec.Members))
	for _, m := range tenant.Spec.Members {
		desired[strings.ToLower(m.Email)] = true
	}
	tracked := make(map[string]hatchetv1alpha1.MemberStatus, len(tenant.Status.Members))
	next := make([]hatchetv1alpha1.MemberStatus, 0, len(tenant.Spec.Members))
	for _, st := range tenant.Status.Members {
		tracked[strings.ToLower(st.Email)] = st
		if desired[strings.ToLower(st.Email)] {
			continue
		}
		if err := deleteAccess(ctx, hc, tenantID, st.Email, access); err != nil {
			fail("member "+st.Email, err)
			next = append(next, st)
		}
	}

	for _, m := range tenant.Spec.Members {
		st, err := r.ensureMember(ctx, hc, tenant, m, access)
		if err != nil {
			fail("member "+m.Email, err)
			if old, ok := tracked[strings.ToLower(m.Email)]; ok {
				next = append(next, old)
			}
			continue
		}
		next = append(next, st)
	}

	tenant.Status.Members = next
	return first
}

// ensureMember invites one person or aligns the role of their member or pending invite, and returns the status entry.
func (r *HatchetTenantReconciler) ensureMember(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant,
	m hatchetv1alpha1.Member, access tenantAccess) (hatchetv1alpha1.MemberStatus, error) {
	tenantID := tenant.Status.TenantID
	email := strings.ToLower(m.Email)
	st := hatchetv1alpha1.MemberStatus{Email: m.Email, State: hatchetv1alpha1.MemberStateInvited}

	if member, ok := access.members[email]; ok {
		st.State = hatchetv1alpha1.MemberStateMember
		if member.Role != m.Role {
			if err := hc.UpdateMember(ctx, tenantID, member.ID, m.Role); err != nil {
				return st, err
			}
		}
		return st, nil
	}

	if invites := access.invites[email]; len(invites) > 0 {
		for _, invite := range invites {
			if invite.Role != m.Role {
				if err := hc.UpdateInvite(ctx, tenantID, invite.ID, m.Role); err != nil {
					return st, err
				}
			}
		}
		st.InviteExpiresAt = &metav1.Time{Time: invites[0].Expires}
		return st, nil
	}

	if err := hc.CreateInvite(ctx, tenantID, m.Email, m.Role); err != nil {
		return st, err
	}
	r.Recorder.Eventf(tenant, nil, corev1.EventTypeNormal, "MemberInvited", "InviteMember", "Invited %s as %s", m.Email, m.Role)
	return st, nil
}

// deleteAccess removes the member and every pending invite of an email; missing ones are ignored.
func deleteAccess(ctx context.Context, hc hatchet.Client, tenantID, email string, access tenantAccess) error {
	key := strings.ToLower(email)
	if member, ok := access.members[key]; ok {
		if err := hc.DeleteMember(ctx, tenantID, member.ID); err != nil && !errors.Is(err, hatchet.ErrNotFound) {
			return err
		}
	}
	for _, invite := range access.invites[key] {
		if err := hc.DeleteInvite(ctx, tenantID, invite.ID); err != nil && !errors.Is(err, hatchet.ErrNotFound) {
			return err
		}
	}
	return nil
}

// deleteTrackedMembers removes every person recorded in status.
func (r *HatchetTenantReconciler) deleteTrackedMembers(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant) error {
	if len(tenant.Status.Members) == 0 {
		return nil
	}
	tenantID := tenant.Status.TenantID
	access, err := listAccess(ctx, hc, tenantID)
	if err != nil {
		return scopedError("members", hatchetv1alpha1.ReasonMemberFailed, hatchetError(err))
	}
	for _, st := range tenant.Status.Members {
		if err := deleteAccess(ctx, hc, tenantID, st.Email, access); err != nil {
			return scopedError("member "+st.Email, hatchetv1alpha1.ReasonMemberFailed, hatchetError(err))
		}
	}
	return nil
}
