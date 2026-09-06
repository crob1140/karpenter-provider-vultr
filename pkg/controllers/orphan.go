package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	orphanScanInterval = 5 * time.Minute
	orphanGracePeriod  = 20 * time.Minute
)

// OrphanController is a safety net for the narrow failure window between
// creating a Vultr instance and successfully persisting/observing the
// corresponding NodeClaim. Karpenter's own NodeClaim liveness/termination
// controllers remain authoritative for normal launch and registration
// failures; this controller only removes instances that have lost their
// NodeClaim entirely and have aged past the grace period.
type OrphanController struct {
	client      client.Client
	vultr       *vultr.Client
	clusterName string
	now         func() time.Time
}

func NewOrphanController(c client.Client, v *vultr.Client, clusterName string) *OrphanController {
	return &OrphanController{client: c, vultr: v, clusterName: clusterName, now: time.Now}
}

func (r *OrphanController) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	instances, err := r.vultr.ListInstances(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("listing Vultr instances for orphan cleanup: %w", err)
	}

	for i := range instances {
		instance := &instances[i]
		// Only ever consider instances tagged for this specific Karpenter
		// installation. Other clusters may share the Vultr account.
		nodeClaimName, ok := vultr.NodeClaimName(instance, r.clusterName)
		if !ok {
			continue
		}

		created, err := time.Parse(time.RFC3339, instance.DateCreated)
		if err != nil || r.now().Sub(created) < orphanGracePeriod {
			continue
		}

		nodeClaim := &karpv1.NodeClaim{}
		err = r.client.Get(ctx, types.NamespacedName{Name: nodeClaimName}, nodeClaim)
		if err == nil {
			// A deleting NodeClaim is still expected to be handled by
			// Karpenter's normal termination controller. Do not race it.
			continue
		}
		if client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("checking NodeClaim %q: %w", nodeClaimName, err)
		}

		if err := r.vultr.DeleteInstance(ctx, instance.ID); err != nil {
			if apiErr, ok := err.(*vultr.APIError); ok && apiErr.NotFound() {
				continue
			}
			return reconcile.Result{}, fmt.Errorf("deleting orphan Vultr instance %q: %w", instance.ID, err)
		}
	}

	return reconcile.Result{RequeueAfter: orphanScanInterval}, nil
}

// SetupWithManager registers the orphan cleaner as a singleton controller.
//
// controller-runtime rejects a controller with no configured watches
// ("there are no watches configured, controller will never get triggered"), so
// this uses the same singleton event source Karpenter's own periodic
// controllers use: one synthetic event at startup, after which the reconciler's
// RequeueAfter drives the scan interval.
func (r *OrphanController) SetupWithManager(m manager.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		Named("vultr-orphan-cleaner").
		WatchesRawSource(singleton.Source()).
		Complete(r)
}
