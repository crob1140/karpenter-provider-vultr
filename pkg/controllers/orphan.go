package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	nodeClaimTagPrefix = "karpenter-nodeclaim="
)

// OrphanController is a safety net for the narrow failure window between
// creating a Vultr instance and successfully persisting/observing the
// corresponding NodeClaim. Karpenter's own NodeClaim liveness/termination
// controllers remain authoritative for normal launch and registration
// failures; this controller only removes instances that have lost their
// NodeClaim entirely and have aged past the grace period.
type OrphanController struct {
	client client.Client
	vultr  *vultr.Client
	now    func() time.Time
}

func NewOrphanController(c client.Client, v *vultr.Client) *OrphanController {
	return &OrphanController{client: c, vultr: v, now: time.Now}
}

func (r *OrphanController) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	instances, err := r.vultr.ListInstances(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("listing Vultr instances for orphan cleanup: %w", err)
	}

	for i := range instances {
		instance := &instances[i]
		nodeClaimName, ok := managedNodeClaim(instance.Tags)
		if !ok || nodeClaimName == "" {
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

func (r *OrphanController) SetupWithManager(m manager.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		Named("vultr-orphan-cleaner").
		Complete(r)
}

func managedNodeClaim(tags []string) (string, bool) {
	for _, tag := range tags {
		if strings.HasPrefix(tag, nodeClaimTagPrefix) {
			value := strings.TrimPrefix(tag, nodeClaimTagPrefix)
			if value == "" || strings.ContainsAny(value, " \t\r\n") {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}
