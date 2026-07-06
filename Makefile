# Include ODC common make targets
DEV_KIT_VERSION := v1.0.10
-include common.mk
common.mk:
	curl --fail -sSL https://raw.githubusercontent.com/opendefensecloud/dev-kit/$(DEV_KIT_VERSION)/common.mk -o common.mk.download && \
	mv common.mk.download $@

APIGEN ?= $(LOCALGOBIN)/apigen

KCP ?= $(LOCALBIN)/kcp
KCP_VERSION ?= 0.32.3

IMG_REGISTRY ?= ghcr.io/opendefense
IMG_TAG ?= latest
CONTROLLER_IMG ?= $(IMG_REGISTRY)/krop-controller:$(IMG_TAG)

TIMESTAMP := $(shell date '+%Y%m%d%H%M%S')
DEV_TAG ?= dev.$(TIMESTAMP)
export DEV_TAG

LICENSE := apache
LICENSE_COMMENT := opendefense contributors

##@ Development

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy methods.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: $(CONTROLLER_GEN) $(APIGEN) ## Generate the engine CRD and convert to kcp APIResourceSchemas + APIExport.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=config/crds
	$(APIGEN) --input-dir=config/crds --output-dir=config/kcp

.PHONY: fmt
fmt: $(ADDLICENSE) $(GOLANGCI_LINT) ## Add license headers and format code.
	$(MAKE) addlicense license=$(LICENSE) comment='$(LICENSE_COMMENT)' pattern='*\.go'
	$(GO) fmt ./...
	$(GOLANGCI_LINT) run --fix

.PHONY: lint
lint: lint-no-golangci golangci-lint ## Run all linters.

.PHONY: lint-no-golangci
lint-no-golangci: ## Run linters except golangci-lint (license headers + shellcheck).
	$(MAKE) addlicense-check license=$(LICENSE) comment='$(LICENSE_COMMENT)' pattern='*\.go'

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

##@ Build

.PHONY: build
build: generate ## Build the controller binary.
	$(GO) build -o $(LOCALBIN)/krop-controller ./cmd/controller/

.PHONY: run
run-controller: generate ## Run the controller from source.
	$(GO) run ./cmd/controller/

.PHONY: docker-build
docker-build: ## Build the Docker image (native single-arch; multi-arch is the CI pipeline's job).
	$(DOCKER) build -t $(CONTROLLER_IMG) .

KCP_CONTROLLER_IMG ?= $(IMG_REGISTRY)/krop-controller-kcp:$(IMG_TAG)

.PHONY: docker-build-kcp
docker-build-kcp: ## Build the kcp-mode Docker image (cmd/controller).
	$(DOCKER) build -f Dockerfile.kcp -t $(KCP_CONTROLLER_IMG) .

.PHONY: docker-push
docker-push: ## Push the Docker image.
	$(DOCKER) push $(CONTROLLER_IMG)

.PHONY: helm-package
helm-package: manifests ## Package Helm chart.
	helm package charts/access-operator

.PHONY: deploy-standalone
deploy-standalone: ## Apply the fog/edge standalone deployment (plain cluster, non-kcp).
	kubectl apply -k config/standalone

##@ Testing

.PHONY: test
test: $(GINKGO) $(KCP) generate ## Run all tests (excludes e2e).
	TEST_KCP_ASSETS=$(LOCALBIN) $(GINKGO) -r -cover --fail-fast --require-suite -covermode count --output-dir=$(BUILD_PATH) -coverprofile=coverprofile --skip-package=test/e2e $(testargs)

.PHONY: test-e2e
test-e2e: $(GINKGO) ## Run e2e tests (kind + kcp + helm). Set E2E_SHARD_CONFIG=single-shard|multi-shard (default: multi-shard).
	$(GINKGO) -r --fail-fast -v --timeout 30m ./test/e2e/ $(testargs)

.PHONY: test-e2e-matrix
test-e2e-matrix: ## Run e2e tests against both shard configs (single-shard, multi-shard).
	$(MAKE) clean-e2e
	E2E_SHARD_CONFIG=single-shard $(MAKE) test-e2e
	$(MAKE) clean-e2e
	E2E_SHARD_CONFIG=multi-shard  $(MAKE) test-e2e

.PHONY: e2e-cleanup
clean-e2e: ## Remove kind cluster from e2e tests.
	-$(KIND) delete cluster --name access-op-e2e 2>/dev/null

##@ Tool Dependencies

.PHONY: $(KCP)
$(KCP): $(LOCALBIN) ## Download kcp binary locally if necessary.
	@test -s $(LOCALBIN)/kcp && $(LOCALBIN)/kcp --version 2>&1 | grep -q "$(KCP_VERSION)" || (\
	echo "Downloading kcp v$(KCP_VERSION) for $(OS)/$(ARCH)..."; \
	curl -fsSL -o $(LOCALBIN)/kcp.tar.gz "https://github.com/kcp-dev/kcp/releases/download/v$(KCP_VERSION)/kcp_$(KCP_VERSION)_$(OS)_$(ARCH).tar.gz"; \
	tar -xzf $(LOCALBIN)/kcp.tar.gz -C $(LOCALBIN) --strip-components=1 bin/; \
	rm -f $(LOCALBIN)/kcp.tar.gz; \
	chmod +x $(LOCALBIN)/kcp)
