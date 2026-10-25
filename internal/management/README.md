# SSH management identity

The management worker keeps its host private key at
`StateDir/ssh_host_ed25519_key`. Enabling SSH explicitly initializes a missing
key with Ed25519 in the existing unencrypted PKCS#8 PEM codec. A malformed
existing key is an error; startup never repairs or replaces it. Existing
supported unencrypted encodings, including OpenSSH, are read without conversion.
Identity codec version 1 is an ABI declaration, not a new header in key files.

Key reads require a worker-owned 0400/0600 singly linked, nonempty regular file of at
most 64 KiB, containing exactly one unencrypted private key with no extra PEM
block or trailing content. Ancestors are pinned without following symlinks and
must belong to root or the worker, with no group/other writes except a root-owned
sticky ancestor. FIFO/device inputs are opened nonblocking and rejected by type.
Parse errors do not echo private contents. The shared `internal/statefs` path
walker is also used by network state and WireGuard readers; WireGuard's separate
secret-directory policy is unchanged.

New keys are written to an exclusive random 0600 temporary file in that pinned
parent, synchronized, closed and published by `RENAME_NOREPLACE`, then the parent
is synchronized. A concurrent creator reads the published winner rather than
returning its own discarded generated signer. An error after publication reports
`ErrIdentityUncertain` and retains the final file. Retrying inspects that file
and synchronizes its parent again; it never deletes a published identity.
A process crash before publication can leave a private temporary orphan; it is
ignored and retained, not mistaken for the key or automatically removed.

`InspectHostKey(ctx, path)` is read-only. It creates no key or directory, performs
no durability sync and reports only presence, codec version, public algorithm and
public-key fingerprint. It does not export a digest of private bytes. Only an
absent leaf in a checked existing parent can report `present:false`; an
uninspectable parent or malformed key is an error. Inspection is an observation,
not permission to regenerate a lost established identity. The initializing
`HostKey` operation remains distinct from inspection.

The signed PID 1 fixture inspects explicit absence with SSH disabled. The
persistent ext4 fixture inspects the actual management key during both boots and
compares its public fingerprint with the SSH peer. These checks do not implement
operator key rotation/revocation, cross-worker state snapshots, supported paired
runtime/configuration boot or physical power-loss qualification. Snapshots must
not restore old identity keys or undo revocation policy. Tests of process-exit
publication boundaries are separate from write-time power-cut/ENOSPC/fsync tests.

Scratch key encodings are cleared where practical. This is not encrypted storage
or a guarantee that Go runtime and crypto-library copies are erased.
