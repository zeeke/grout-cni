BINDIR ?= bin
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w
KIND_CLUSTER ?= grout-cni-e2e
KIND_NODE_IMAGE ?= kindest/node:grout-cni-v1.32.2
KIND_CONFIG ?= deploy/kind-config.yaml
MULTUS_MANIFEST ?= https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/v4.1.0/deployments/multus-daemonset.yml
E2E_POD_IMAGE ?= busybox:1.36
TESTPMD_IMAGE ?= grout-cni-testpmd:e2e

.PHONY: all build test e2e lint image clean kind-node-image kind-e2e kind-setup kind-teardown

all: build

build: $(BINDIR)/grout-cni

$(BINDIR)/grout-cni: $(shell find cmd/grout-cni pkg -name '*.go')
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/grout-cni

test:
	go test ./...

e2e:
	go test -v -tags=e2e ./test/e2e/...

lint:
	golangci-lint run ./...

image: build
	podman build -t grout-cni -f deploy/Dockerfile.cni .

kind-node-image: build
	docker build -t $(KIND_NODE_IMAGE) -f deploy/Dockerfile.kind-node .

kind-setup: kind-node-image
	kind create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --config $(KIND_CONFIG)

kind-e2e:
	@echo "=== Verifying kind cluster ==="
	kubectl cluster-info
	kubectl get nodes -o wide
	@echo "=== Deploying grout DaemonSet ==="
	kubectl apply -f deploy/grout-daemonset.yaml
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

kind-teardown:
	-kind delete cluster --name $(KIND_CLUSTER)

clean:
	rm -rf $(BINDIR)
