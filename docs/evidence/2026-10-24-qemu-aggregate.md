# 同一checkpointの全20 QEMU系統

2026-10-24 UTC。固定code checkpoint: `c2f3612fa96bd66e89f4d190d06d90e6b37fbc6d`。

cleanなdetached worktreeで以下を一度のMake実行として完走し、exit status 0を確認した。
kernel/modulesは既存Alpine imageから取り出し、initializer、試験binary、daemonのELFは
このworktreeのsourceから各gateで作り直した。外部NICや本番ディスクは使っていない。

```sh
umask 022
make qemu-network qemu-protocols qemu-wireguard qemu-ra-dns \
  qemu-daemon qemu-daemon-aftr qemu-release-boot qemu-storage \
  IMAGE_DIR=<workspace>/dist/image
```

**全20系統がPASS。** 個別manifestの結果とMake完走の両方を確認した。
先行の中断runや別commitの個別PASSを合成したものではない。
[aggregate manifest](2026-10-24-qemu-aggregate/aggregate.json) と
[実行log](2026-10-24-qemu-aggregate/aggregate.log) に結果を保存する。

| 範囲 | 結果 |
| --- | --- |
| network route/ACL/NAT/DS-Lite/link/RA expiry | PASS |
| ResourceProgram実worker lifecycle | PASS |
| 二WAN Health→PBR、TCP/UDP/mark/PCAP、worker・supervisor再起動、旧通知、expiry、confirmed rollback | PASS、74.42秒 |
| SSH設定、confirmed rollback、logging/control再起動 | PASS |
| DHCP→DNS、legacy DHCP/DNS移行、BGP→FIB | 3系統PASS |
| BFD→BGP、同じBGP workerの障害復旧、停止したBFD producerの期限切れ | PASS、51.60秒 |
| DHCPv4/DHCPv6-PD/RA、DHCP server、VRRP、RA client、AFTR、BFD | 6系統PASS |
| WireGuard実暗号化TCP、RA学習DNS | 2系統PASS |
| 実PID 1の署名trial起動・自動確認 | PASS、102.99秒 |
| private ext4 diskの二回起動・設定/SSH/sentinel保持・未確認trial fallback | PASS、147.70秒 |

各manifestにはkernel・initializer・test/daemon ELFのhashを保持した。
storageではprivate SSH identityのhash/fingerprintを公開せず、二回起動での一致判定と
boot ID間の対応を記録した。原manifestのhashはaggregateに保存する。
[公開artifactのSHA256SUMS](2026-10-24-qemu-aggregate/SHA256SUMS) を参照。

このcheckpointのdurable eventは保存componentまでで、後続の設定outbox接続を含まない。
全20系統は**現在存在する試験範囲**の合格であり、未実装機能、arm64実boot、
物理NIC・性能、kernel/rootfs A/B更新、クラウド・HA実機受入の合格ではない。
全実装計画・対象実機での利用可能性という継続ゴールは未完了のまま維持する。
