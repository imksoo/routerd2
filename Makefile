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
DHCP6_SERVER ?= /usr/sbin/dhcpd
PPP_ARCH ?= $(shell $(GO) env GOARCH)
PPP_DIR ?= dist
UPLINK_INTERFACE ?=
UPLINK_AFTR ?=
UPLINK_SECONDS ?= 7800
UPLINK_DNS_UPSTREAM ?=
QEMU_SSH_KEY ?=

LDFLAGS = -s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:$(RELEASE_ID):END
BUILD_ARGS = --build-arg GO_VERSION=$(GO_VERSION) --build-arg ALPINE_VERSION=$(ALPINE_VERSION) --build-arg RELEASE_ID=$(RELEASE_ID)

.PHONY: help fmt fmt-check test test-dependencies test-tools race vet check check-pppoe check-dual-wan check-health-pbr check-vrrp-unicast check-bgp check-bfd check-pd-ra check-dhcp6 check-aftr check-ra-client check-ra-aftr check-dhcp-relay licenses build build-linux release-built-test container container-multi container-smoke apk image qemu-smoke qemu-network qemu-wireguard qemu-ra-dns qemu-daemon qemu-daemon-aftr qemu-dhcp4-clock qemu-dhcp6-clock qemu-ra-clock qemu-vrrp-daemon qemu-vrrp-activation qemu-vrrp-conditional qemu-vrrp-clock qemu-eventrule qemu-derived qemu-protocols qemu-release-boot qemu-storage qemu-health-power clean

help:
	@printf '%s\n' \
	  'make check             Format, unit tests, race detector, and vet' \
	  'make check-dual-wan    Verify PPPoE/DS-Lite Health/PBR and TCP MSS with independent peers' \
	  'make check-pppoe        Verify real PPPoE PAP/CHAP, LAN forwarding and recovery' \
	  'make check-pppoe-pd     Verify PPP DHCPv6 PD and LAN IPv6 with independent ISC DHCP' \
	  'make check-health-pbr  Required two-WAN real-daemon TCP/UDP failover gate' \
	  'make check-vrrp-unicast Verify VRRP peer failover and finite WAN identity' \
	  'make check-bfd         Verify BFD/BGP routing domains with real workers' \
	  'make check-dhcp6       Verify native IA_NA with independent dnsmasq' \
	  'make check-pd-ra       Verify PD renewal, generated RA and client traffic' \
	  'make check-bgp         Verify actual BGP FIB, reporting failure and ownership expiry' \
	  'make check-aftr        Verify AFTR input identity, DNS and tunnel expiry' \
	  'make check-ra-client   Verify real RA, source/default-route TCP/UDP and expiry' \
	  'make check-ra-aftr     Verify learned RA/DNS to DS-Lite LAN forwarding and expiry' \
	  'make check-ra-route-refresh Verify routed AFTR TCP/UDP during independent RA renewal' \
	  'make check-uplink UPLINK_INTERFACE=... UPLINK_AFTR=...  Use an explicit local uplink with a private MAC/LAN' \
	  'make check-dhcp-relay  Verify DHCP peers, fragmentation and VRF isolation' \
	  'make licenses          Collect dependency licenses and their manifest' \
	  'make build             Build the native commands for the current platform' \
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
	  'make qemu-ra-clock     Verify original RA budgets during guest UTC corrections' \
	  'make qemu-dhcp6-clock Verify original IA_NA/PD/RA expiry during guest UTC steps' \
	  'make qemu-dhcp4-clock  Verify DHCP lease authority during actual guest UTC steps' \
	  'make qemu-vrrp-daemon  Verify original VRRP VIP authority and real daemon recovery' \
	  'make qemu-vrrp-activation Verify graceful election and readiness-gated VIP publication' \
	  'make qemu-vrrp-conditional Verify conditional VRRP effects and dependent withdrawal' \
	  'make qemu-vrrp-clock   Verify VRRP VIP expiry during actual guest UTC steps' \
	  'make qemu-protocols    Verify native protocols and real daemon recovery in QEMU' \
	  'make qemu-eventrule    Verify durable rules and worker/source recovery in QEMU' \
	  'make qemu-derived      Verify finite live status, derived output and recovery' \
	  'make qemu-release-boot Verify signed PID 1 selection and confirmation in QEMU' \
	  'make qemu-storage      Verify persistent state and signed fallback across two boots' \
	  'make qemu-health-power Verify live queue/ACK recovery after two VM power cuts' \
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
	umask 022; $(GO) test ./...
	umask 022; $(GO) test ./internal/releaseboot/testdata/guest

test-dependencies:
	$(GO) test github.com/google/nftables/expr

test-tools:
	$(PYTHON) -m unittest discover -s scripts -p '*_test.py'

race:
	umask 022; CGO_ENABLED=1 $(GO) test -race ./...
	umask 022; CGO_ENABLED=1 $(GO) test -race ./internal/releaseboot/testdata/guest

vet:
	$(GO) vet ./...
	$(GO) vet ./internal/releaseboot/testdata/guest

check: fmt-check test test-dependencies test-tools race vet

check-health-pbr: build
	$(PYTHON) scripts/daemon-tests.py health-pbr --go '$(GO)'

check-dual-wan: build pppoe-engine
	$(PYTHON) scripts/dual-wan-peer.py

.PHONY: check-dual-wan-bridge
check-dual-wan-bridge: build pppoe-engine
	$(PYTHON) scripts/dual-wan-peer.py --bridge-wan --output dist/dual-wan-bridge

check-pppoe: build build-publisher pppoe-engine
	$(PYTHON) scripts/pppoe-peer.py

.PHONY: check-pppoe-pd
check-pppoe-pd: build build-publisher pppoe-engine
	$(PYTHON) scripts/pppoe-peer.py --dhcp6-server '$(DHCP6_SERVER)' --output dist/pppoe-pd

.PHONY: pppoe-engine
pppoe-engine:
	$(DOCKER) build --platform linux/$(PPP_ARCH) --build-arg ALPINE_VERSION=$(ALPINE_VERSION) -f packaging/alpine/pppoe-engine.Dockerfile -t routerd2-pppoe-engine:$(ALPINE_VERSION)-$(PPP_ARCH) .
	mkdir -p $(PPP_DIR)
	@set -eu; profile_id=$$($(DOCKER) create --network none routerd2-pppoe-engine:$(ALPINE_VERSION)-$(PPP_ARCH) /not-executed); \
	  trap '$(DOCKER) rm "$$profile_id" >/dev/null' EXIT HUP INT TERM; \
	  $(DOCKER) export "$$profile_id" | tar --no-same-owner -xf - -C $(PPP_DIR) pppoe-engine.tar pppoe-sources.tar

check-vrrp-unicast: build
	$(PYTHON) scripts/vrrp-unicast.py --go '$(GO)'

check-bgp: build
	$(PYTHON) scripts/daemon-tests.py bgp --go '$(GO)'

check-bfd: build
	$(PYTHON) scripts/daemon-tests.py bfd --go '$(GO)'

check-dhcp6: build
	$(PYTHON) scripts/daemon-tests.py dhcp6 --go '$(GO)'

.PHONY: check-dhcp-dns
check-dhcp-dns: build
	$(PYTHON) scripts/daemon-tests.py dhcp-server --go '$(GO)'

check-pd-ra: build
	$(PYTHON) scripts/daemon-tests.py pd-ra --go '$(GO)'

check-ra-aftr: build
	$(PYTHON) scripts/daemon-tests.py ra-aftr --go '$(GO)'

.PHONY: check-uplink
check-uplink: build
	@test -n '$(UPLINK_INTERFACE)' -a -n '$(UPLINK_AFTR)' || { printf '%s\n' 'UPLINK_INTERFACE and UPLINK_AFTR must select the authorized local access network'; exit 1; }
	$(PYTHON) scripts/uplink-smoke.py --uplink '$(UPLINK_INTERFACE)' --aftr '$(UPLINK_AFTR)' --seconds '$(UPLINK_SECONDS)' --dns-upstream '$(UPLINK_DNS_UPSTREAM)'

.PHONY: check-ra-route-refresh
check-ra-route-refresh: build
	$(PYTHON) scripts/ra-route-refresh.py

check-ra-client: build
	$(PYTHON) scripts/daemon-tests.py ra-client --go '$(GO)'

check-aftr: build
	$(PYTHON) scripts/daemon-tests.py aftr --go '$(GO)'

check-dhcp-relay: build
	$(PYTHON) scripts/daemon-tests.py dhcp-relay --go '$(GO)'

licenses:
	$(PYTHON) scripts/licenses.py --go '$(GO)' --output dist/licenses

build: licenses
	mkdir -p dist/bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o dist/bin/routerd ./cmd/routerd
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o dist/bin/routerd-modules ./cmd/routerd-modules
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o dist/bin/routerd-logging ./cmd/routerd-logging

.PHONY: build-publisher
build-publisher: licenses
	mkdir -p dist/bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o dist/bin/routerd-bundle ./cmd/routerd-bundle

build-linux: licenses
	@set -eu; for arch in amd64 arm64; do \
	  mkdir -p "dist/bin/linux-$$arch"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o "dist/bin/linux-$$arch/routerd" ./cmd/routerd; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o "dist/bin/linux-$$arch/routerd-modules" ./cmd/routerd-modules; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o "dist/bin/linux-$$arch/routerd-logging" ./cmd/routerd-logging; \
	done

release-built-test:
	ROUTERD2_INTEGRATION_REQUIRED=1 ROUTERD2_RELEASE_BUILT_DIR='$(abspath $(RELEASE_BUILT_DIR))' ROUTERD2_RELEASE_BUILT_ID='$(RELEASE_ID)' $(GO) test -race -tags integration -run '^Test(BuiltRuntimeCatalog|PublishBuiltRelease)$$' -count=1 -v ./internal/release ./internal/releasepublish

container: pppoe-engine
	$(DOCKER) build $(BUILD_ARGS) -t $(CONTAINER_TAG) .

container-multi:
	$(MAKE) pppoe-engine PPP_ARCH=amd64 PPP_DIR=dist/ppp-amd64
	$(MAKE) pppoe-engine PPP_ARCH=arm64 PPP_DIR=dist/ppp-arm64
	mkdir -p dist
	$(DOCKER) buildx build $(BUILD_ARGS) --platform linux/amd64,linux/arm64 --output type=oci,dest=dist/routerd2.oci.tar .

container-smoke: container
	$(PYTHON) images/container-smoke.py --engine '$(DOCKER)' --image $(CONTAINER_TAG)

apk: pppoe-engine
	$(DOCKER) build $(BUILD_ARGS) -f packaging/alpine/Dockerfile --target apk-output --output type=local,dest=dist/apk .

image: pppoe-engine
	$(DOCKER) build $(BUILD_ARGS) --build-arg IMAGE_CONFIG=$(IMAGE_CONFIG) --build-arg SSH_AUTHORIZED_KEY -f packaging/alpine/Dockerfile --target appliance-output --output type=local,dest=$(IMAGE_DIR) .

qemu-smoke:
	$(PYTHON) scripts/qemu-smoke.py --image-dir $(IMAGE_DIR) --timeout $(QEMU_TIMEOUT) $(if $(QEMU_SSH_KEY),--ssh-key $(QEMU_SSH_KEY),)

.PHONY: qemu-pppoe
qemu-pppoe:
	$(PYTHON) scripts/qemu-pppoe-peer.py --image-dir $(IMAGE_DIR) --dhcp6-server '$(DHCP6_SERVER)'

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

qemu-ra-clock:
	$(PYTHON) scripts/qemu-network.py --suite daemon-ra-clock --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-dhcp6-clock:
	$(PYTHON) scripts/qemu-network.py --suite daemon-dhcp6-clock --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-dhcp4-clock:
	$(PYTHON) scripts/qemu-network.py --suite daemon-dhcp4-clock --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-vrrp-daemon:
	$(PYTHON) scripts/qemu-network.py --suite daemon-vrrp --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-vrrp-activation:
	$(PYTHON) scripts/qemu-network.py --suite daemon-vrrp-activation --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-vrrp-conditional:
	$(PYTHON) scripts/qemu-network.py --suite daemon-vrrp-conditional --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-vrrp-clock:
	$(PYTHON) scripts/qemu-network.py --suite daemon-vrrp-clock --go '$(GO)' --image-dir $(IMAGE_DIR)

qemu-protocols:
	@set -eu; for suite in boot-address daemon-dhcp4 daemon-dhcp4-clock daemon-dhcp6-clock daemon-ra-clock daemon-vrrp daemon-vrrp-activation daemon-vrrp-conditional daemon-vrrp-clock daemon-resource-program daemon-health-pbr daemon-management daemon-startup daemon-startup-recovery daemon-eventrule daemon-event-archive daemon-derived daemon-stopped-health daemon-dhcp-dns daemon-legacy-dhcp-dns daemon-bgp daemon-bgp-derived daemon-bfd daemon-bfd-derived dhcp dhcp-server vrrp ra-client aftr bfd; do \
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

qemu-health-power:
	$(PYTHON) scripts/qemu-health-power.py --go '$(GO)' --image-dir $(IMAGE_DIR) --output dist/health-power-test --timeout $(QEMU_STORAGE_TIMEOUT)

clean:
	rm -rf dist
