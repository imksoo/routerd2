GO ?= go
DOCKER ?= docker
PYTHON ?= python3
RELEASE_ID ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf 'dev')
GO_VERSION ?= 1.27.3
ALPINE_VERSION ?= 3.24.3
IMAGE_CONFIG ?= empty
IMAGE_DIR ?= dist/image
CONTAINER_TAG ?= routerd2:dev
QEMU_TIMEOUT ?= 90
QEMU_DAEMON_TIMEOUT ?= 180
QEMU_RELEASE_TIMEOUT ?= 240
QEMU_STORAGE_TIMEOUT ?= 240
RELEASE_BUILT_DIR ?= dist/bin
QEMU_SSH_KEY ?=

LDFLAGS = -s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:$(RELEASE_ID):END
BUILD_ARGS = --build-arg GO_VERSION=$(GO_VERSION) --build-arg ALPINE_VERSION=$(ALPINE_VERSION) --build-arg RELEASE_ID=$(RELEASE_ID)

.PHONY: help fmt fmt-check test test-dependencies test-tools race vet check check-health-pbr licenses build build-linux release-built-test container container-multi container-smoke apk image qemu-smoke qemu-network qemu-wireguard qemu-ra-dns qemu-daemon qemu-daemon-aftr qemu-eventrule qemu-derived qemu-protocols qemu-release-boot qemu-storage clean

help:
	@printf '%s\n' \
	  'make check             Format, unit tests, race detector, and vet' \
	  'make check-health-pbr  Required two-WAN real-daemon TCP/UDP failover gate' \
	  'make licenses          Collect dependency licenses and their manifest' \
	  'make build             Build both binaries for the current platform' \
	  'make build-linux       Build static linux/amd64 and linux/arm64 binaries' \
	  'make release-built-test Verify actual amd64/arm64 release bundle identities' \
	  'make apk               Test and build the Alpine package with abuild' \
	  'make image             Export Alpine kernel + routerd initramfs' \
	  'make qemu-smoke        Boot the image in QEMU; optionally verify SSH' \
	  'make qemu-network      Run native network tests in a separate QEMU guest' \
	  'make qemu-wireguard    Verify kernel WireGuard encrypted traffic and ownership' \
	  'make qemu-ra-dns       Verify learned RA DNS expiry and live updates in QEMU' \
	  'make qemu-daemon       Run the two-binary RA daemon gate in QEMU' \
	  'make qemu-daemon-aftr  Run the two-binary AFTR daemon gate in QEMU' \
	  'make qemu-protocols    Verify native protocols and real daemon recovery in QEMU' \
	  'make qemu-eventrule    Verify durable rules and worker/source recovery in QEMU' \
	  'make qemu-derived      Verify finite live status, derived output and recovery' \
	  'make qemu-release-boot Verify signed PID 1 selection and confirmation in QEMU' \
	  'make qemu-storage      Verify persistent state and signed fallback across two boots' \
	  'make container         Build the scratch OCI runtime image' \
	  'make container-smoke   Run the container and query its control API' \
	  'make container-multi   Export an amd64/arm64 OCI archive'

fmt:
	$(GO) fmt ./...

fmt-check:
	@set -eu; formatter="$$($(GO) env GOROOT)/bin/gofmt"; \
	  unformatted=$$(find . -type f -name '*.go' -not -path './dist/*' -exec "$$formatter" -l '{}' +); \
	  if [ -n "$$unformatted" ]; then printf '%s\n' "$$unformatted"; printf '%s\n' 'Run make fmt.'; exit 1; fi

test:
	$(GO) test ./...

test-dependencies:
	$(GO) test github.com/google/nftables/expr

test-tools:
	$(PYTHON) -m unittest discover -s scripts -p '*_test.py'

race:
	CGO_ENABLED=1 $(GO) test -race ./...

vet:
	$(GO) vet ./...

check: fmt-check test test-dependencies test-tools race vet

check-health-pbr: build
	$(PYTHON) scripts/health-pbr.py --go '$(GO)'

licenses:
	$(PYTHON) scripts/licenses.py --go '$(GO)' --output dist/licenses

build: licenses
	mkdir -p dist/bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o dist/bin/routerd ./cmd/routerd
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o dist/bin/routerd-modules ./cmd/routerd-modules

build-linux: licenses
	@set -eu; for arch in amd64 arm64; do \
	  mkdir -p "dist/bin/linux-$$arch"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o "dist/bin/linux-$$arch/routerd" ./cmd/routerd; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o "dist/bin/linux-$$arch/routerd-modules" ./cmd/routerd-modules; \
	done

release-built-test:
	ROUTERD2_INTEGRATION_REQUIRED=1 ROUTERD2_RELEASE_BUILT_DIR='$(abspath $(RELEASE_BUILT_DIR))' ROUTERD2_RELEASE_BUILT_ID='$(RELEASE_ID)' $(GO) test -race -tags integration -run '^Test(BuiltReleasePairs|PublishBuiltRelease)$$' -count=1 -v ./internal/release ./internal/releasepublish

container:
	$(DOCKER) build $(BUILD_ARGS) -t $(CONTAINER_TAG) .

container-multi:
	mkdir -p dist
	$(DOCKER) buildx build $(BUILD_ARGS) --platform linux/amd64,linux/arm64 --output type=oci,dest=dist/routerd2.oci.tar .

container-smoke: container
	$(PYTHON) images/container-smoke.py --engine $(DOCKER) --image $(CONTAINER_TAG)

apk:
	$(DOCKER) build $(BUILD_ARGS) -f packaging/alpine/Dockerfile --target apk-output --output type=local,dest=dist/apk .

image:
	$(DOCKER) build $(BUILD_ARGS) --build-arg IMAGE_CONFIG=$(IMAGE_CONFIG) --build-arg SSH_AUTHORIZED_KEY -f packaging/alpine/Dockerfile --target appliance-output --output type=local,dest=$(IMAGE_DIR) .

qemu-smoke:
	$(PYTHON) scripts/qemu-smoke.py --image-dir $(IMAGE_DIR) --timeout $(QEMU_TIMEOUT) $(if $(QEMU_SSH_KEY),--ssh-key $(QEMU_SSH_KEY),)

qemu-network:
	$(PYTHON) scripts/qemu-network.py --go '$(GO)' --image-dir $(IMAGE_DIR) --timeout $(QEMU_TIMEOUT)

qemu-wireguard:
	$(PYTHON) scripts/qemu-network.py --suite wireguard --go '$(GO)' --image-dir $(IMAGE_DIR) --timeout 120

qemu-ra-dns:
	$(PYTHON) scripts/qemu-network.py --suite ra-dns --go '$(GO)' --image-dir $(IMAGE_DIR) --timeout 75

qemu-daemon:
	$(PYTHON) scripts/qemu-network.py --suite daemon --go '$(GO)' --image-dir $(IMAGE_DIR) --timeout $(QEMU_DAEMON_TIMEOUT)

qemu-daemon-aftr:
	$(PYTHON) scripts/qemu-network.py --suite daemon-aftr --go '$(GO)' --image-dir $(IMAGE_DIR) --timeout $(QEMU_DAEMON_TIMEOUT)

qemu-protocols:
	@set -eu; for suite in daemon-resource-program daemon-health-pbr daemon-management daemon-eventrule daemon-derived daemon-stopped-health daemon-dhcp-dns daemon-legacy-dhcp-dns daemon-bgp daemon-bfd dhcp dhcp-server vrrp ra-client aftr bfd; do \
	  $(PYTHON) scripts/qemu-network.py --suite "$$suite" --go '$(GO)' --image-dir $(IMAGE_DIR); \
	done

qemu-eventrule:
	$(PYTHON) scripts/qemu-network.py --suite daemon-eventrule --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-derived:
	$(PYTHON) scripts/qemu-network.py --suite daemon-derived --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-release-boot:
	$(PYTHON) internal/releaseboot/testdata/qemu-release-boot.py --go '$(GO)' --image-dir $(IMAGE_DIR) --output dist/release-boot-test --timeout $(QEMU_RELEASE_TIMEOUT)

qemu-storage:
	$(PYTHON) scripts/qemu-storage.py --go '$(GO)' --image-dir $(IMAGE_DIR) --output dist/storage-test --timeout $(QEMU_STORAGE_TIMEOUT)

clean:
	rm -rf dist
