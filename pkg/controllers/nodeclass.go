package controllers

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
)

type NodeClassController struct{ client client.Client }

func NewNodeClassController(c client.Client) *NodeClassController {
	return &NodeClassController{client: c}
}
func (r *NodeClassController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nc := &vultrv1.VultrNodeClass{}
	if err := r.client.Get(ctx, req.NamespacedName, nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	before := nc.DeepCopy()
	conds := nc.StatusConditions()
	switch {
	case nc.Spec.Region == "":
		conds.SetFalse(status.ConditionReady, "InvalidRegion", "spec.region must be set")
	case nc.Spec.OSID == nil && nc.Spec.SnapshotID == "":
		conds.SetFalse(status.ConditionReady, "ImageNotConfigured", "set spec.osID or spec.snapshotID")
	case nc.Spec.OSID != nil && nc.Spec.SnapshotID != "":
		conds.SetFalse(status.ConditionReady, "MultipleImagesConfigured", "set only one of spec.osID or spec.snapshotID")
	default:
		conds.SetTrue(status.ConditionReady)
	}
	nc.Status.ObservedGeneration = nc.Generation
	if !conditionsEqual(before.Status.Conditions, nc.Status.Conditions) || before.Status.ObservedGeneration != nc.Status.ObservedGeneration {
		if err := r.client.Status().Update(ctx, nc); err != nil {
			return reconcile.Result{}, fmt.Errorf("updating VultrNodeClass status: %w", err)
		}
	}
	return reconcile.Result{}, nil
}
func (r *NodeClassController) SetupWithManager(m manager.Manager) error {
	return ctrl.NewControllerManagedBy(m).For(&vultrv1.VultrNodeClass{}).Complete(r)
}
func (r *NodeClassController) Register(_ context.Context, _ manager.Manager) error { return nil }
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Status != b[i].Status || a[i].Reason != b[i].Reason || a[i].Message != b[i].Message || a[i].ObservedGeneration != b[i].ObservedGeneration {
			return false
		}
	}
	return true
}
