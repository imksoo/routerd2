# Alpine package

`make apk` builds `routerd2-0.1.0-r0.apk` in an Alpine 3.24.3 build environment
using the official Go 1.27.3 toolchain. `abuild` runs as the unprivileged
`builder` user; its `check()` phase runs `CGO_ENABLED=0 go test ./...`.

The APK installs:

- `/usr/sbin/routerd`
- `/usr/libexec/routerd-modules`
- `/etc/routerd/config.json` with an empty network configuration
- `/etc/routerd/boot.json` for explicit appliance startup
- `/usr/share/licenses/routerd2/LICENSE`

It has no runtime package dependencies or init scripts and does not replace an
existing host's init. Only the custom appliance assembly creates `/init`.

The local source archive, completed APKBUILD with checksums, public signing key,
and toolchain version are exported with the APK to `dist/apk`. A temporary
builder key signs development packages. Release builds need a separately
managed signing key and trust policy; a development key is not a production
update trust root.

The build wrapper generates the source archive and runs `abuild checksum`
before `abuild -d -r`. The `-d` switch skips dependency installation because the
Docker build explicitly provides alpine-sdk and the pinned official Go
toolchain. The source APKBUILD still declares its Go build requirement for
native Alpine use. The generated archive and APKBUILD can be used together in
an Alpine package build environment.
