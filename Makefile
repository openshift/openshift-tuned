PACKAGE=github.com/openshift/openshift-tuned
PACKAGE_BIN=$(lastword $(subst /, ,$(PACKAGE)))
PACKAGE_SRC=$(wildcard cmd/*.go)

# Build-specific variables
OUT_DIR=_output
GO=GO111MODULE=on GOFLAGS=-mod=vendor go
GOFMT_CHECK=$(shell find . -not \( \( -wholename './.*' -o -wholename '*/vendor/*' \) -prune \) -name '*.go' | sort -u | xargs gofmt -s -l)
REV=$(shell git describe --long --tags --match='v*' --always --dirty)

# Container image-related variables
DOCKERFILE=Dockerfile
IMAGE_TAG=openshift/openshift-tuned
IMAGE_REGISTRY=quay.io

all: $(PACKAGE_BIN)

$(PACKAGE_BIN) build: $(PACKAGE_SRC)
	$(GO) build -o $(OUT_DIR)/$(PACKAGE_BIN) -ldflags '-X main.version=$(REV)' $^

vet: $(PACKAGE_SRC)
	$(GO) vet -printfuncs=Info,Infof,Warning,Warningf $^

verify:	verify-gofmt

verify-gofmt:
ifeq (, $(GOFMT_CHECK))
	@echo "verify-gofmt: OK"
else
	@echo "verify-gofmt: ERROR: gofmt failed on the following files:"
	@echo "$(GOFMT_CHECK)"
	@echo ""
	@echo "For details, run: gofmt -d -s $(GOFMT_CHECK)"
	@echo ""
	@exit 1
endif

test:
	$(GO) test ./cmd/... -coverprofile cover.out

clean:
	$(GO) clean
	rm -rf $(OUT_DIR)

local-image:
ifdef USE_BUILDAH
	buildah bud $(BUILDAH_OPTS) -t $(IMAGE_TAG) -f $(DOCKERFILE) .
else
	sudo docker build -t $(IMAGE_TAG) -f $(DOCKERFILE) .
endif

local-image-push:
ifdef USE_BUILDAH
	buildah push $(BUILDAH_OPTS) $(IMAGE_TAG) $(IMAGE_REGISTRY)/$(IMAGE_TAG)
else
	sudo docker tag $(IMAGE_TAG) $(IMAGE_REGISTRY)/$(IMAGE_TAG)
	sudo docker push $(IMAGE_REGISTRY)/$(IMAGE_TAG)
endif

.PHONY: all build run fmt format vet clean local-image local-image-push
