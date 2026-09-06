package main

import (
	"os"
	"strings"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"

	vultrprovider "github.com/crob1140/karpenter-provider-vultr/pkg/cloudprovider"
	vultrcontrollers "github.com/crob1140/karpenter-provider-vultr/pkg/controllers"
	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

func main() {
	ctx, op := operator.NewOperator()
	apiKey := os.Getenv("VULTR_API_KEY")
	if apiKey == "" {
		panic("VULTR_API_KEY is required")
	}
	// Vultr instances carry no cluster identity of their own. Every instance is
	// tagged with this name, and the provider only ever lists, adopts or
	// deletes instances carrying it, so that two Karpenter installations
	// sharing a Vultr account cannot garbage-collect each other's nodes.
	clusterName := strings.TrimSpace(os.Getenv("CLUSTER_NAME"))
	if clusterName == "" {
		panic("CLUSTER_NAME is required: it scopes Vultr instance ownership to this Karpenter installation")
	}

	vultrClient := vultr.NewClient(apiKey)
	rawProvider := vultrprovider.New(op.GetClient(), vultrClient, clusterName)
	decoratedProvider := metrics.Decorate(rawProvider)
	cloudProvider := overlay.Decorate(decoratedProvider, op.GetClient(), op.InstanceTypeStore)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	op.WithControllers(ctx,
		controllers.NewControllers(ctx, op.Manager, op.Clock, op.GetClient(), op.EventRecorder, cloudProvider, rawProvider, clusterState, op.InstanceTypeStore)...,
	)
	if err := vultrcontrollers.NewNodeClassController(op.GetClient(), vultrClient).SetupWithManager(op.Manager); err != nil {
		panic(err)
	}
	if err := vultrcontrollers.NewOrphanController(op.GetClient(), vultrClient, clusterName).SetupWithManager(op.Manager); err != nil {
		panic(err)
	}
	op.Start(ctx)
}
