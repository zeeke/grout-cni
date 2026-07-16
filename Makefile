BINDIR ?= bin
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w
KIND_CLUSTER ?= grout-cni-e2e
KIND_NODE_IMAGE ?= kindest/node:grout-cni-v1.32.2
KIND_CONFIG ?= deploy/kind-config.yaml
MULTUS_MANIFEST ?= https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/v4.1.0/deployments/multus-daemonset.yml
E2E_POD_IMAGE ?= busybox:1.36
TESTPMD_IMAGE ?= grout-cni-testpmd:e2e
GROUT_VERSION ?= 0.16.0
GROUT_IMAGE ?= quay.io/grout/grout:$(GROUT_VERSION)

.PHONY: all build test e2e lint image clean kind-node-image kind-e2e kind-setup kind-teardown release-check release-snapshot help

all: build

build: $(BINDIR)/grout-cni ## Build the CNI binary (static, CGO_ENABLED=0)

$(BINDIR)/grout-cni: $(shell find cmd/grout-cni pkg -name '*.go')
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/grout-cni

test: ## Run unit tests
	go test ./...

e2e: ## Run integration tests (requires Docker + grout container)
	GROUT_IMAGE=$(GROUT_IMAGE) go test -v -tags=e2e ./test/e2e/...

lint: ## Run golangci-lint
	golangci-lint run ./...

image: build ## Build the container image via podman
	podman build -t grout-cni -f deploy/Dockerfile.cni .

kind-node-image: build ## Build the kind node image with the CNI binary
	docker build -t $(KIND_NODE_IMAGE) -f deploy/Dockerfile.kind-node .

kind-setup: kind-node-image ## Create a kind cluster for e2e testing
	kind create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --config $(KIND_CONFIG)

kind-e2e: ## Run full Kubernetes e2e (requires a running kind cluster)
	@echo "=== Verifying kind cluster ==="
	kubectl cluster-info
	kubectl get nodes -o wide
	@echo "=== Deploying grout DaemonSet ($(GROUT_IMAGE)) ==="
	kubectl apply -f deploy/grout-daemonset.yaml
	kubectl -n grout-system set image daemonset/grout grout=$(GROUT_IMAGE)
	kubectl rollout status daemonset/grout -n grout-system --timeout=240s
	kubectl get pods -n grout-system -o wide
	@echo "=== Installing Multus (secondary-CNI scenario) ==="
	kubectl apply -f $(MULTUS_MANIFEST)
	kubectl rollout status daemonset/kube-multus-ds -n kube-system --timeout=240s
	@echo "=== Applying NetworkAttachmentDefinitions ==="
	kubectl apply -f deploy/nad-tap.yaml
	kubectl apply -f deploy/nad-virtio.yaml
	kubectl apply -f deploy/nad-mixed.yaml
	kubectl apply -f deploy/nad-dual.yaml
	@echo "=== Preloading test workload images into the cluster ==="
	docker pull $(E2E_POD_IMAGE)
	kind load docker-image $(E2E_POD_IMAGE) --name $(KIND_CLUSTER)
	docker build -t $(TESTPMD_IMAGE) -f deploy/Dockerfile.testpmd deploy
	kind load docker-image $(TESTPMD_IMAGE) --name $(KIND_CLUSTER)
	@echo "=== Running k8s e2e tests ==="
	go test -v -tags=k8se2e ./test/k8se2e/...

kind-teardown: ## Delete the kind e2e cluster
	-kind delete cluster --name $(KIND_CLUSTER)

release-check: ## Validate the GoReleaser configuration
	goreleaser check

release-snapshot: ## Dry-run the release pipeline locally (no publish, no tag needed)
	REGISTRY_IMAGE=ghcr.io/zeeke/grout-cni goreleaser release --snapshot --clean

clean: ## Remove build artifacts
	rm -rf $(BINDIR) dist

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
