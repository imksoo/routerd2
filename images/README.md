# Images and boot validation

The appliance uses an Alpine Linux kernel and a custom initramfs. `/init` is a
symlink to the static `routerd` binary. The root filesystem contains the
`routerd` and `routerd-modules` executables, configuration, CA certificates,
license text, and Linux modules. It contains no shell, OpenRC, systemd, apk,
modprobe, or decompression programs. PID 1 mounts the virtual filesystems and
uses routerd's native kernel module loader. Worker processes execute the
`routerd-modules` binary under the supervisor.

Alpine's package tools are build dependencies. `packaging/alpine/Dockerfile`
runs the test suite through `abuild`, builds the routerd APK, and installs that
APK into a staging root. A separate image stage installs `linux-virt`, extracts
the required files, decompresses the kernel modules, and regenerates
`modules.dep`. The build publishes its kernel release and input package hash in
`manifest.json`.

For development inside an Alpine image-builder environment,
`scripts/build-image.sh --binary-dir DIR --output DIR` accepts the two existing
binaries directly. Its manifest says `input_type: development-binaries` and
contains no package hash. This is useful for rapid boot debugging and does not
replace the APK build/test gate used by `make image` and CI.

## Build an appliance

Requirements: Docker with BuildKit. QEMU and Python 3 are needed only for the
smoke test.

```sh
make apk
make image
make qemu-smoke
```

Artifacts are `dist/image/vmlinuz`, `initramfs.cpio.gz`, `manifest.json`, and
`SHA256SUMS`. The default image has empty network configuration and SSH disabled.
It does not configure the build host or expose an unauthenticated management
service.

`linux-virt` is a VM-oriented kernel. This initial image is validated on QEMU
x86_64 using virtio networking. Native amd64 and arm64 routerd binaries and OCI
images are built separately; the arm64 appliance boot path and physical NIC
drivers are not yet validated.

## QEMU fixture with the embedded SSH CLI

The QEMU fixture assigns `10.0.2.15/24` to `eth0` and uses QEMU's user-mode
gateway `10.0.2.2`. Generate a disposable key and inject only its public half:

```sh
ssh-keygen -q -t ed25519 -N '' -C routerd2-qemu -f /tmp/routerd2-qemu
export SSH_AUTHORIZED_KEY="$(cat /tmp/routerd2-qemu.pub)"
make image IMAGE_CONFIG=qemu
make qemu-smoke QEMU_SSH_KEY=/tmp/routerd2-qemu
```

The test uses TCG so it also works without KVM. It waits for `ROUTERD_READY`,
fails on a kernel panic or early exit, then runs `show status`, `show network`,
`show config`, and `show version` through the embedded SSH server. It verifies
PID 1, release identities, the configured IPv4 address, and recovery of the
committed network revision after restarting its worker. Its random
loopback-only forwarded port and temporary known-hosts file do not change host
SSH settings. Serial output is retained as `dist/image/qemu-serial.log`.

The generated fixture contains the test public key and fixed test addressing.
Do not deploy it as a production configuration. The private key is never copied
into an image or uploaded as a CI artifact.

## Container profile

```sh
make container
make container-smoke
make container-multi
```

The final OCI stage is `scratch` and explicitly starts `routerd run --platform
container`. It contains both executables and CA certificates, with no Alpine
userspace. The smoke test creates an isolated network namespace, grants
`NET_ADMIN` and `NET_RAW`, uses a read-only root with writable runtime/state
tmpfs mounts, queries the control API, and checks graceful shutdown.

The container does not boot a kernel. The host must provide the required Linux
network features and kernel modules. This repository does not install a CNI,
join a Kubernetes cluster, replace kube-proxy, or claim an operational SDN
deployment simply by building this OCI image.

## Current persistence and boot limits

This image boots directly with a kernel plus initramfs. Its state directory is
in RAM: generated SSH host keys and other state are lost on reboot. Persistent
storage provisioning, mounting a data partition, firmware-specific boot,
Secure Boot enrollment, a bootloader, A/B slot selection, boot-success marking,
and rollback are not implemented by this image builder.

No disk image, installer ISO, cloud image, or cloud-provider integration is
produced. The manifest records `volatile-initramfs` and
`bootloader_ab_updates: false` so direct-boot smoke success is not confused with
an update-safe physical appliance.

Alpine and Go patch versions are pinned in the build arguments. The kernel
package is resolved from the pinned Alpine release branch and its exact version
is recorded; fully frozen package mirrors and base-image digests are a later
reproducible-release step.
