TOOL = go tool -modfile=tools/go.mod
IMG ?= hatchet-controller:dev
GOLANG_VERSION ?= $(shell awk '/^golang/ {print $$2}' .tool-versions)
HATCHET_VERSIONS ?= v0.84.0 v0.98.12 v0.112.10

.PHONY: all
all: build

.PHONY: install
install:
	yarn install
	go mod download
	go mod download -modfile=tools/go.mod

.PHONY: install-immutable
install-immutable:
	yarn install --immutable
	go mod download
	go mod verify
	go mod download -modfile=tools/go.mod
	go mod verify -modfile=tools/go.mod

.PHONY: generate
generate:
	$(TOOL) controller-gen object paths=./api/...

.PHONY: manifests
manifests:
	$(TOOL) controller-gen crd rbac:roleName=manager-role paths=./... \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: build
build: generate manifests
	go build -o bin/manager ./cmd

.PHONY: lint
lint:
	$(TOOL) golangci-lint run --build-tags integration,e2e

.PHONY: vulncheck
vulncheck:
	$(TOOL) govulncheck ./...

.PHONY: check
check: lint vulncheck
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt: files need formatting" && exit 1)
	$(MAKE) generate manifests
	git diff --exit-code

.PHONY: test-unit
test-unit:
	KUBEBUILDER_ASSETS="$$($(TOOL) setup-envtest use -p path)" go test $$(go list ./... | grep -v -e /test/integration -e /test/e2e)

.PHONY: test-integration
test-integration:
	HATCHET_VERSIONS="$(HATCHET_VERSIONS)" KUBEBUILDER_ASSETS="$$($(TOOL) setup-envtest use -p path)" \
		go test -tags integration -timeout 30m ./test/integration/...

.PHONY: test-e2e
test-e2e:
	IMG=$(IMG) go test -tags e2e -timeout 30m ./test/e2e/...

.PHONY: docker-build
docker-build:
	docker build --build-arg golang_version=$(GOLANG_VERSION) -t $(IMG) .
