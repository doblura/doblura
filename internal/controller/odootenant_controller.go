// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooTenantReconciler keeps the customer record true, and counts consumption.
//
// Two jobs, and the second is the interesting one.
//
// The count of open ephemeral environments existed in the API and was written by
// nothing, so the quota webhook could not read it and had to list environments
// itself on every admission. Writing it here makes the customer record answer the
// question a human asks it.
//
// The consumption counter is a different kind of thing: it is the only value in
// this project that must be **monotonic and durable**, because it is the one an
// invoice would be built from. That has two consequences the rest of the codebase
// does not have to think about.
//
// First, it is accrued against a watermark rather than recomputed. Every other
// status here is "what is true now", and recomputing those is what makes them
// self-healing. A total of what was consumed cannot be recomputed, because the
// environments that consumed it are deleted — so it is only ever added to, from
// LastAccountedAt to now, and a restart resumes from the persisted watermark
// instead of starting again at zero.
//
// Second, it is honestly approximate, and the approximation is bounded and
// documented rather than hidden:
//
//   - An environment created and destroyed entirely between two passes contributes
//     nothing. The error is at most one interval per environment, which is why the
//     interval is a minute and not an hour.
//   - Time before ReadyAt is not counted. Restoring 40 GiB takes minutes to hours
//     and nobody should be billed for a snapshot they could not open.
//   - Clock changes on the node move the watermark, not the total.
//
// An accounting record that is approximately right and says by how much is worth
// more than one that is exactly wrong.
type OdooTenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AccountEvery is how often consumption is accrued. It bounds the undercount
	// above; a minute keeps it negligible against TTLs measured in hours.
	AccountEvery time.Duration
}

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odootenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odootenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=odooenvironments,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoodatabases,verbs=get;list;watch

func (r *OdooTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tenant doblurav1alpha1.OdooTenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := tenant.Status.DeepCopy()
	st.ObservedGeneration = tenant.Generation

	var envs doblurav1alpha1.OdooEnvironmentList
	if err := r.List(ctx, &envs, client.InNamespace(tenant.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var dbs doblurav1alpha1.OdooDatabaseList
	if err := r.List(ctx, &dbs, client.InNamespace(tenant.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	// ── What is true now ──
	st.Databases, st.SharedDatabases = countTenantDatabases(&dbs, tenant.Name)
	st.EphemeralEnvironments = countOpenEphemeral(&envs, tenant.Name)

	// ── What was consumed ──
	now := metav1.Now()
	accrued := accrueMilliHours(&envs, tenant.Name, st.LastAccountedAt, now)
	st.EnvironmentMilliHours += accrued
	st.LastAccountedAt = &now

	// Study each catalogue entry once, and once again if it is repointed. A
	// failure here is recorded on the entry rather than failing the tenant: the
	// customer record is not broken because an image would not start, and the
	// fact that it would not start is exactly the report somebody wants.
	for _, entry := range tenant.Spec.Images {
		if err := r.ensureImageStudy(ctx, &tenant, st, entry); err != nil {
			log.FromContext(ctx).Error(err, "could not study an image",
				"tenant", tenant.Name, "entry", entry.Name)
		}
	}

	quota := tenant.Spec.EphemeralQuota()
	cond := metav1.Condition{
		Type: doblurav1alpha1.ConditionWithinQuota, Status: metav1.ConditionTrue,
		Reason:  "WithinQuota",
		Message: quotaMessage(st.EphemeralEnvironments, quota),
	}
	if st.EphemeralEnvironments >= quota {
		cond.Status, cond.Reason = metav1.ConditionFalse, "AtQuota"
	}
	meta.SetStatusCondition(&st.Conditions, cond)

	every := r.AccountEvery
	if every <= 0 {
		every = time.Minute
	}
	if !equality.Semantic.DeepEqual(&tenant.Status, st) {
		tenant.Status = *st
		if err := r.Status().Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: every}, nil
}

// accrueMilliHours adds up what the tenant's environments consumed since the
// watermark, weighted by size class.
//
// The weights track the memory-request ladder the operator already applies, so the
// meter and the resources it is charging for come from the same place. Getting this
// wrong in the flattering direction is how a platform loses money on precisely its
// heaviest users.
func accrueMilliHours(
	envs *doblurav1alpha1.OdooEnvironmentList,
	tenant string,
	since *metav1.Time,
	now metav1.Time,
) int64 {
	// With no watermark this is the first pass ever. Accruing from ReadyAt would
	// silently invoice history that predates the meter, so the first pass
	// establishes the watermark and counts nothing.
	if since == nil {
		return 0
	}
	var total int64
	for i := range envs.Items {
		e := &envs.Items[i]
		if e.Spec.ForTenant != tenant || e.Status.ReadyAt == nil {
			continue
		}
		// The interval this environment was actually consuming within
		// [since, now]: it starts no earlier than when it became ready and ends no
		// later than when it terminated.
		from := since.Time
		if e.Status.ReadyAt.After(from) {
			from = e.Status.ReadyAt.Time
		}
		to := now.Time
		if t := e.Status.TerminatedAt; t != nil && t.Time.Before(to) {
			to = t.Time
		}
		if !to.After(from) {
			continue
		}
		ms := to.Sub(from).Milliseconds()
		total += ms * sizeWeightMilli(e.Spec.Size) / (1000 * 3600)
	}
	return total
}

// sizeWeightMilli is the size class as thousandths, so the arithmetic above stays
// in integers.
//
// small 0.5, medium 1, large 3 — following the memory-request ladder rather than a
// round guess, and defaulting to medium for an unset size because that is what the
// operator itself defaults the workload to.
func sizeWeightMilli(size doblurav1alpha1.Size) int64 {
	switch size {
	case doblurav1alpha1.SizeSmall:
		return 500
	case doblurav1alpha1.SizeLarge:
		return 3000
	default:
		return 1000
	}
}

// countOpenEphemeral counts what the quota is measured against.
//
// "Open" means it has a TTL and has not terminated. A hibernated environment still
// counts: it holds its database and its disk, which is what the quota is protecting.
func countOpenEphemeral(envs *doblurav1alpha1.OdooEnvironmentList, tenant string) int32 {
	var n int32
	for i := range envs.Items {
		e := &envs.Items[i]
		if e.Spec.ForTenant != tenant {
			continue
		}
		if e.Spec.Lifecycle.TTL == nil {
			continue // persistent: staging is not a throwaway
		}
		if e.Status.TerminatedAt != nil {
			continue
		}
		n++
	}
	return n
}

func countTenantDatabases(dbs *doblurav1alpha1.OdooDatabaseList, tenant string) (total, shared int32) {
	for i := range dbs.Items {
		d := &dbs.Items[i]
		var mine bool
		for _, c := range d.Spec.Companies {
			if c.TenantRef == tenant {
				mine = true
				break
			}
		}
		if !mine {
			continue
		}
		total++
		if d.Spec.Tenancy == doblurav1alpha1.TenancyShared {
			shared++
		}
	}
	return total, shared
}

// quotaMessage says where the customer stands, in the words the webhook uses.
func quotaMessage(open, quota int32) string {
	return fmt.Sprintf("%d of %d ephemeral environments open", open, quota)
}

// tenantsInNamespace maps an environment or database back to every tenant in its
// namespace.
//
// Every tenant, rather than the one named by spec.forTenant: a database declares
// its tenants through spec.companies, an environment through spec.forTenant, and
// resolving each shape separately would mean two mapping functions that can drift.
// Namespaces here hold a handful of tenants, so the extra reconciles are cheaper
// than the divergence.
func tenantsInNamespace(mgr ctrl.Manager) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			var tenants doblurav1alpha1.OdooTenantList
			if err := mgr.GetClient().List(ctx, &tenants,
				client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			out := make([]reconcile.Request, 0, len(tenants.Items))
			for i := range tenants.Items {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tenants.Items[i])})
			}
			return out
		})
}

func (r *OdooTenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watches environments and databases as well as tenants: the counts above are
	// about them, and waiting for the accounting tick to notice a new environment
	// would leave the quota condition stale for up to a minute.
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooTenant{}).
		Watches(&doblurav1alpha1.OdooEnvironment{}, tenantsInNamespace(mgr)).
		Watches(&doblurav1alpha1.OdooDatabase{}, tenantsInNamespace(mgr)).
		Named("odootenant").
		Complete(r)
}
