IMAGE ?= ghcr.io/crob1140/karpenter-provider-vultr:dev
CONTROLLER_GEN ?= controller-gen

.PHONY: build test test-integration generate docker-build manifests
build:
	go build ./cmd/controller

test:
	go test ./...

# The envtest suite is opt-in. Without VULTR_INTEGRATION_TESTS=1 every test in
# it skips, so set it here rather than relying on the caller.
test-integration:
	VULTR_INTEGRATION_TESTS=1 go test ./pkg/integration/...

generate:
	$(CONTROLLER_GEN) object paths="./pkg/apis/..."
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:artifacts:config=config/crd

docker-build:
	docker build -t $(IMAGE) .

manifests:
	kubectl apply -f config/crd/vultrnodeclass.yaml
	kubectl apply -f config/rbac/serviceaccount.yaml
	kubectl apply -f config/rbac/vultr-nodeclass-rbac.yaml
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/rbac/rolebinding.yaml
	kubectl apply -f config/deployment.yaml
