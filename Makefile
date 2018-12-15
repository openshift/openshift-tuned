PROGRAM=openshift-tuned
SRC_DIR=cmd
BIN_DIR=./
DOCKERFILE=Dockerfile
IMAGE_TAG=openshift/openshift-tuned
IMAGE_REGISTRY=docker.io
GOFMT_CHECK=$(shell find . -not \( \( -wholename './.*' -o -wholename '*/vendor/*' \) -prune \) -name '*.go' | sort -u | xargs gofmt -s -l)
REV=$(shell git describe --long --tags --match='v*' --always --dirty)

all: $(BIN_DIR)/$(PROGRAM)

$(BIN_DIR)/$(PROGRAM) build: $(SRC_DIR)/$(PROGRAM).go
	go build -o $(BIN_DIR)/$(PROGRAM) -ldflags '-X main.version=$(REV)' $<

run: $(SRC_DIR)/$(PROGRAM).go
	go run $<

vet: $(SRC_DIR)/$(PROGRAM).go
	go tool vet -shadow=false -printfuncs=Info,Infof,Warning,Warningf $<

strip:
	strip $(BIN_DIR)/$(PROGRAM)

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
	go test ./cmd/... -coverprofile cover.out

clean:
	go clean

local-image:
ifdef USE_BUILDAH
	buildah bud -t $(IMAGE_TAG) -f $(DOCKERFILE) .
else
	docker build -t $(IMAGE_TAG) -f $(DOCKERFILE) .
endif

local-image-push:
	buildah push $(IMAGE_TAG) $(IMAGE_REGISTRY)/$(IMAGE_TAG)

.PHONY: all build run fmt format vet strip clean local-image local-image-push
