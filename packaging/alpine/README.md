# Alpine package

`make apk` builds `routerd2-0.1.0-r0.apk` in an Alpine 3.24.3 build environment
using the official Go 1.27.3 toolchain. `abuild` runs as the unprivileged
`builder` user; its `check()` phase runs `CGO_ENABLED=0 go test ./...`.
The check phase sets both `TMPDIR` and `GOTMPDIR` to `/tmp` so Go's private
test directories keep Unix socket paths within the kernel's length limit.
Go 1.27's `testing.TempDir` uses `GOTMPDIR`; changing `TMPDIR` alone does not
override abuild's nested `aports` directory.
Alpine's `-trimpath` and PIE defaults remain in the test environment: release
tests locate the supplied Go toolchain through `PATH` when trimmed binaries
have no compiled-in GOROOT, and build their signed ELF fixtures explicitly with
`-buildmode=exe`, matching the appliance executables. Tests and readiness
assertions are not skipped or relaxed for the package build.

The APK installs:

- `/usr/sbin/routerd`
- `/usr/libexec/routerd-modules`
- `/etc/routerd/config.json` with an empty network configuration
- `/etc/routerd/boot.json` for explicit appliance startup
- `/usr/share/licenses/routerd2/LICENSE`
- `/usr/share/licenses/routerd2/dependencies/` with dependency license texts,
  notices, and their manifest

It has no runtime package dependencies or init scripts and does not replace an
existing host's init. Only the custom appliance assembly creates `/init`.

The local source archive, completed APKBUILD with checksums, public signing key,
and toolchain version are exported with the APK to `dist/apk`. A temporary
builder key signs development packages. Release builds need a separately
managed signing key and trust policy; a development key is not a production
update trust root.

The build wrapper generates the source archive and runs `abuild checksum`
before `abuild -d -r`. The `-d` switch skips dependency installation because the
Docker build explicitly provides alpine-sdk, Python 3, and the pinned official
Go toolchain. The source APKBUILD still declares its Go and Python requirements
for native Alpine use. The generated archive and APKBUILD can be used together in
an Alpine package build environment.

The package build runs `scripts/licenses.py` against its source module graph;
failure to collect a dependency license fails the build. These collected texts
are carried into the appliance instead of adding a runtime package manager.
