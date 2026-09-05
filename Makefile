IMAGE ?= ghcr.io/crob1140/karpenter-provider-vultr:dev
CONTROLLER_GEN ?= controller-gen

.PHONY: build test generate docker-build manifests
build:
	go build ./cmd/controller

test:
	go test ./...

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
