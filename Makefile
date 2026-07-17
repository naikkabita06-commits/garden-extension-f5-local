# SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

ENSURE_GARDENER_MOD         := $(shell go get github.com/gardener/gardener@$$(go list -m -f "{{.Version}}" github.com/gardener/gardener))
GARDENER_HACK_DIR           := $(shell go list -m -f "{{.Dir}}" github.com/gardener/gardener)/hack
EXTENSION_PREFIX            := gardener-extension
NAME                        := f5
REGISTRY                    := europe-docker.pkg.dev/gardener-project/public/gardener
IMAGE_PREFIX                := $(REGISTRY)/extensions
REPO_ROOT                   := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
VERSION                     := $(shell cat "$(REPO_ROOT)/VERSION")
EFFECTIVE_VERSION           := $(VERSION)-$(shell git rev-parse HEAD)
LD_FLAGS                    := "-w $(shell bash $(GARDENER_HACK_DIR)/get-build-ld-flags.sh k8s.io/component-base $(REPO_ROOT)/VERSION $(EXTENSION_PREFIX))"

.PHONY: start
start:
	@go run -ldflags $(LD_FLAGS) ./cmd/$(EXTENSION_PREFIX)-$(NAME)

.PHONY: build
build:
	@go build -ldflags $(LD_FLAGS) -o bin/$(EXTENSION_PREFIX)-$(NAME) ./cmd/$(EXTENSION_PREFIX)-$(NAME)

.PHONY: docker-build
docker-build:
	@docker build -t $(IMAGE_PREFIX)/$(NAME):$(EFFECTIVE_VERSION) .

.PHONY: docker-push
docker-push:
	@docker push $(IMAGE_PREFIX)/$(NAME):$(EFFECTIVE_VERSION)

.PHONY: helm-package
helm-package:
	@helm package charts/gardener-extension-f5 -d .

.PHONY: gen-controllerdeployment
gen-controllerdeployment:
	@IMAGE_REPOSITORY=$(IMAGE_PREFIX)/$(NAME) IMAGE_TAG=$(EFFECTIVE_VERSION) OUTPUT=deploy/garden/controllerdeployment-f5.yaml bash scripts/generate-controllerdeployment-f5.sh

.PHONY: install
install:
	@LD_FLAGS=$(LD_FLAGS) EFFECTIVE_VERSION=$(EFFECTIVE_VERSION) bash $(GARDENER_HACK_DIR)/install.sh ./...

.PHONY: format
format:
	@bash $(GARDENER_HACK_DIR)/format.sh ./cmd ./pkg

.PHONY: test
test:
	@bash $(GARDENER_HACK_DIR)/test.sh ./cmd/... ./pkg/...

.PHONY: verify
verify: format test

.PHONY: generate
generate:
	@bash $(GARDENER_HACK_DIR)/generate.sh ./cmd/... ./pkg/...

