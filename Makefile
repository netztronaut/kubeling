APPLICATION := kubeling
CHART_DIR ?= charts/$(APPLICATION)

IMAGE_REPOSITORY ?= git.example.com/platform/$(APPLICATION)
IMAGE_TAG ?= latest
PLATFORMS ?= linux/amd64,linux/arm64

# Optional Helm values file and namespace for `make deploy`, e.g.
# make deploy VALUES=../gitops/kubeling/values.yaml NAMESPACE=kube-system
VALUES ?=
NAMESPACE ?=

HELM ?= helm
LOCALBIN ?= $(CURDIR)/bin
GOLANGCI_LINT_VERSION ?= v2.13.2
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  %-12s %s\n", $$1, $$2 } \
		/^##@/ { printf "\n%s\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: check
check: vet lint test helm-lint helm-test ## Run every check CI runs.

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format Go code.
	$(GOLANGCI_LINT) fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: ## Run Go tests with the race detector and coverage.
	go test -race -cover ./...

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	$(HELM) lint --strict $(CHART_DIR)
	$(HELM) lint --strict $(CHART_DIR) --values=$(CHART_DIR)/ci/rules-values.yaml

.PHONY: helm-test
helm-test: ## Render the Helm chart and assert on the output.
	HELM=$(HELM) hack/test-chart.sh $(CHART_DIR)

.PHONY: build
build: ## Build the kubeling binary into bin/.
	CGO_ENABLED=0 go build -trimpath -o $(LOCALBIN)/$(APPLICATION) ./cmd/$(APPLICATION)

.PHONY: clean
clean: ## Remove build output and downloaded tools.
	rm -rf $(LOCALBIN) cover.out

##@ Release

.PHONY: image
image: ## Build and push the multi-arch container image.
	docker buildx build --platform=$(PLATFORMS) --tag=$(IMAGE_REPOSITORY):$(IMAGE_TAG) --push .

.PHONY: deploy
deploy: ## Install or upgrade the Helm release in the current kube context.
	$(HELM) upgrade --install $(APPLICATION) $(CHART_DIR) \
		$(if $(NAMESPACE),--namespace=$(NAMESPACE)) \
		$(if $(VALUES),--values=$(VALUES)) \
		--set=image.repository=$(IMAGE_REPOSITORY) --set=image.tag=$(IMAGE_TAG)

# Tools

$(GOLANGCI_LINT):
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(LOCALBIN)/golangci-lint $@
