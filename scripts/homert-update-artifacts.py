"""Prepare the signed runtime inputs used by the isolated homert rolling update."""

import json
import shutil
import tarfile


def prepare(stage, boot, overlays, run, *, base_binaries, next_binaries,
            engine_dir, publisher):
    """Reuse the full-profile release packaging and initial-store procedure."""
    # The isolated runner is root. The publisher intentionally refuses input
    # paths writable by another user, so freeze the chosen inputs in its stage.
    inputs = stage / "signed-input"
    inputs.mkdir(mode=0o700)
    fixed = []
    for label, directory in (("base", base_binaries), ("next", next_binaries)):
        target = inputs / label
        target.mkdir(mode=0o700)
        for name in ("routerd", "routerd-modules", "routerd-logging", "ndpi-sources.tar"):
            shutil.copyfile(directory / name, target / name)
            (target / name).chmod(0o500 if name.startswith("routerd") else 0o400)
        fixed.append(target)
    base_binaries, next_binaries = fixed
    for stem in ("pppoe", "dnsmasq", "conntrack", "tailscale", "chrony"):
        for kind in ("engine", "sources"):
            name = stem + "-" + kind + ".tar"
            shutil.copyfile(engine_dir / name, inputs / name)
            (inputs / name).chmod(0o400)
    engine_dir = inputs
    signing = stage / "release-signing.pem"
    run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(signing)])
    signing.chmod(0o600)
    trust = stage / "release-keys.json"
    trust.write_text(run([str(publisher), "trust", "--private-key-file", str(signing)]))
    trust.chmod(0o600)
    engines = {"pppoe": "pppoe", "dhcp_server": "dnsmasq", "vrrp": "conntrack",
               "tailscale": "tailscale", "ntp": "chrony"}
    refs, versions = {}, {}
    abi = None
    for sequence, (label, directory) in enumerate(
            (("base", base_binaries), ("next", next_binaries)), 1):
        artifacts = []
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            version = json.loads(run([str(directory / name), "version"]))
            if abi is None:
                abi = version["runtime_abi"]
            assert version["runtime_abi"] == abi, (label, name, "different runtime contract")
            if label in versions:
                assert version["release_id"] == versions[label], (label, name)
            versions[label] = version["release_id"]
            artifacts.append({"name": name, "kind": "native", "command": name,
                              "file": str(directory / name)})
        for role, stem in engines.items():
            for kind in ("engine", "sources"):
                artifacts.append({"name": role + "-" + kind, "kind": "data",
                                  "file": str(engine_dir / (stem + "-" + kind + ".tar"))})
        artifacts.append({"name": "ndpi-sources", "kind": "data",
                          "file": str(directory / "ndpi-sources.tar")})
        layout = {"boot_abi": abi["boot_abi"], "worker_abi": abi["worker_abi"],
                  "entrypoint": "routerd",
                  "roles": [{"role": role, "artifact": "routerd-logging" if role == "logging"
                             else "routerd-modules"} for role in boot["workers"]],
                  "engines": [{"role": role, "artifact": role + "-engine",
                               "sources": role + "-sources"} for role in engines],
                  "artifacts": artifacts}
        path = stage / (label + "-layout.json")
        path.write_text(json.dumps(layout))
        path.chmod(0o600)
        refs[label] = json.loads(run([
            str(publisher), "pack", "--private-key-file", str(signing),
            "--layout", str(path), "--release-id", versions[label],
            "--sequence", str(sequence), "--target-arch", "amd64",
            "--output", str(stage / (label + ".tar"))], 300))
        print("Signed full catalog", label, versions[label], refs[label]["digest"], flush=True)
    assert versions["base"] != versions["next"], "update needs distinct release identities"
    initial = stage / "initial-state"
    initial.mkdir(mode=0o700)
    selector = ["--root", str(initial / "releases"), "--trust-file", str(trust),
                "--target-arch", "amd64"]
    installer = str(base_binaries / "routerd")
    baseline = json.loads(run([installer, "release", "stage", *selector,
                               "--file", str(stage / "base.tar")], 300))
    run([installer, "release", "initialize", *selector, "--digest", baseline["digest"]], 300)
    with tarfile.open(stage / "initial-state.tar", "w") as archive:
        archive.add(initial, arcname=".")
    for overlay in overlays.values():
        shutil.copyfile(trust, overlay / "etc/routerd/release-keys.json")
        (overlay / "etc/routerd/release-keys.json").chmod(0o600)
        path = overlay / "etc/routerd/boot.json"
        selected = json.loads(path.read_text())
        selected.update(workers={}, engines={}, release={
            "root": "/var/lib/routerd/releases", "trust_file": "/etc/routerd/release-keys.json",
            "ready_timeout_seconds": 90, "stable_seconds": 2})
        path.write_text(json.dumps(selected))
    (stage / "signed-refs.json").write_text(json.dumps(
        {"refs": refs, "versions": versions}, indent=2) + "\n")
    return refs, versions
