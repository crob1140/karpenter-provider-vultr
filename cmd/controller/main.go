package main

import (
	"os"

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

	rawProvider := vultrprovider.New(op.GetClient(), vultr.NewClient(apiKey))
	decoratedProvider := metrics.Decorate(rawProvider)
	cloudProvider := overlay.Decorate(decoratedProvider, op.GetClient(), op.InstanceTypeStore)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	op.WithControllers(ctx,
		controllers.NewControllers(ctx, op.Manager, op.Clock, op.GetClient(), op.EventRecorder, cloudProvider, rawProvider, clusterState, op.InstanceTypeStore)...,
	)
	if err := vultrcontrollers.NewNodeClassController(op.GetClient()).SetupWithManager(op.Manager); err != nil {
		panic(err)
	}
	op.Start(ctx)
}
