package controllers

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

// nodeClassRefreshInterval bounds how stale the resolved regional plan
// availability reported in status can become.
const nodeClassRefreshInterval = 5 * time.Minute

var caHashRE = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)

type NodeClassController struct {
	client        client.Client
	vultr         *vultr.Client
	instanceTypes *vultr.InstanceTypeProvider
}

func NewNodeClassController(c client.Client, v *vultr.Client) *NodeClassController {
	return &NodeClassController{client: c, vultr: v, instanceTypes: vultr.NewInstanceTypeProvider(v)}
}

func (r *NodeClassController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nc := &vultrv1.VultrNodeClass{}
	if err := r.client.Get(ctx, req.NamespacedName, nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	before := nc.DeepCopy()
	conds := nc.StatusConditions()
	nc.Status.ObservedGeneration = nc.Generation
	nc.Status.ResolvedPlanIDs = nil

	var requeue bool
	var reason, message string
	switch {
	case strings.TrimSpace(nc.Spec.Region) == "":
		reason, message = "InvalidRegion", "spec.region must be set"
	case nc.Spec.OSID == nil && nc.Spec.SnapshotID == "":
		reason, message = "ImageNotConfigured", "set spec.osID or spec.snapshotID"
	case nc.Spec.OSID != nil && nc.Spec.SnapshotID != "":
		reason, message = "MultipleImagesConfigured", "set only one of spec.osID or spec.snapshotID"
	case nc.Spec.KubernetesVersion == "":
		reason, message = "KubernetesVersionMissing", "spec.kubernetesVersion must be set"
	case nc.Spec.ClusterEndpoint == "":
		reason, message = "ClusterEndpointMissing", "spec.clusterEndpoint must be set"
	case !validEndpoint(nc.Spec.ClusterEndpoint):
		reason, message = "InvalidClusterEndpoint", "spec.clusterEndpoint must be an https URL with a host"
	case nc.Spec.CACertHash != "" && !validCAHash(nc.Spec.CACertHash):
		reason, message = "InvalidCACertHash", "spec.caCertHash must use the format sha256:<64 hex characters>"
	}

	if reason == "" && nc.Spec.OSID != nil {
		osImages, err := r.vultr.ListOS(ctx)
		if err != nil {
			reason, message = "OSLookupFailed", fmt.Sprintf("unable to validate OS ID %d: %v", *nc.Spec.OSID, err)
			requeue = true
		} else {
			found := false
			for _, image := range osImages {
				if image.ID == *nc.Spec.OSID {
					found = true
					if !isAMD64(image.Arch) {
						reason, message = "UnsupportedOSArchitecture", fmt.Sprintf("Vultr OS ID %d has architecture %q; this provider currently supports amd64 Linux workers", *nc.Spec.OSID, image.Arch)
					}
					break
				}
			}
			if !found && reason == "" {
				reason, message = "InvalidOS", fmt.Sprintf("Vultr OS ID %d does not exist or is not available to this account", *nc.Spec.OSID)
			}
		}
	}

	if reason == "" {
		available, err := r.instanceTypes.ValidateRegion(ctx, nc.Spec.Region)
		if err != nil {
			reason, message = "VultrAPIUnavailable", fmt.Sprintf("unable to validate region %q: %v", nc.Spec.Region, err)
			requeue = true
		} else {
			nc.Status.ResolvedPlanIDs = sortedSet(available)
			if len(available) == 0 {
				reason, message = "NoPlansAvailable", fmt.Sprintf("Vultr reports no available compute plans in region %q", nc.Spec.Region)
			} else if nc.Spec.Plan != "" {
				plans, err := r.instanceTypes.Plans(ctx)
				if err != nil {
					reason, message = "PlanLookupFailed", fmt.Sprintf("unable to validate plan %q: %v", nc.Spec.Plan, err)
					requeue = true
				} else if _, ok := plans[nc.Spec.Plan]; !ok {
					reason, message = "InvalidPlan", fmt.Sprintf("Vultr plan %q does not exist", nc.Spec.Plan)
				} else if _, ok := available[nc.Spec.Plan]; !ok {
					reason, message = "PlanUnavailable", fmt.Sprintf("Vultr plan %q is not currently available in region %q", nc.Spec.Plan, nc.Spec.Region)
				}
			}
		}
	}

	if reason != "" {
		conds.SetFalse(status.ConditionReady, reason, message)
	} else {
		conds.SetTrue(status.ConditionReady)
	}

	if !conditionsEqual(before.Status.Conditions, nc.Status.Conditions) || before.Status.ObservedGeneration != nc.Status.ObservedGeneration || !stringSlicesEqual(before.Status.ResolvedPlanIDs, nc.Status.ResolvedPlanIDs) {
		if err := r.client.Status().Update(ctx, nc); err != nil {
			return reconcile.Result{}, fmt.Errorf("updating VultrNodeClass status: %w", err)
		}
	}
	if requeue {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	// Regional plan availability changes without the NodeClass spec changing,
	// so re-resolve periodically instead of only on spec edits. Otherwise a
	// NodeClass keeps reporting availability that Vultr has since withdrawn.
	return reconcile.Result{RequeueAfter: nodeClassRefreshInterval}, nil
}

func (r *NodeClassController) SetupWithManager(m manager.Manager) error {
	return ctrl.NewControllerManagedBy(m).For(&vultrv1.VultrNodeClass{}).Complete(r)
}
func (r *NodeClassController) Register(_ context.Context, _ manager.Manager) error { return nil }

func validEndpoint(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func validCAHash(value string) bool {
	return caHashRE.MatchString(value)
}

func isAMD64(arch string) bool {
	switch strings.ToLower(arch) {
	case "amd64", "x86_64", "x86-64":
		return true
	default:
		return false
	}
}

func sortedSet(in map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for value := range in {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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
