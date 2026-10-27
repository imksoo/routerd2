# routerd2 開発引き継ぎ

2026-10-27。全実装計画のゴールは継続中。対象実機で利用可能とは判断していない。
機能範囲・未完了項目は[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)に従う。[AGENTS.md](../AGENTS.md)を優先する。
過去のAPI・保存形式・試験結果を維持するための実装は残さない。

## 現在の変更: control-drain / worker ABI5 / native-28

operatorの`restart control`で、古いcontrolがDHCPv6の最終撤去を受付・適用する前に
停止していた。独立RP-PPPoE/ISC・永続ext4 state付きの実applianceで再現し、
新control/PPPの起動後も旧PDのReleaseが一度もないことを確認した。
旧routerdの`cmd/routerd/cmd_serve.go`の停止経路も比較した。

supervisorの既存停止順序をcontrol交換にも使う。旧control・network・management・loggingと
WANを維持したままDHCPv6を停止・reapし、残りのdynamic worker、旧controlの順に停止する。
旧producerをretireしてから新controlを起動し、設定されたworkerを再生成する。
二重のcoordinator・registry・旧契約adapter・timeout延長は追加していない。

`Status.Stopping`を`Lifecycle`（`running` / `restarting_control` / `stopping`）へ置換した。
全consumerを更新し、worker ABIを5へ変更した。boot ABI1・manifest v4・network journal9・
compiler native-28は同じ。ABI4の内部メッセージは受け付けない。
202は再起動要求の受付であり、protocol取得や転送の復旧完了ではない。

## 確認した範囲

最終`control-drain`の実装で確認済み:

- PVE Ubuntu Linux6.8/amd64、署名PID1 catalog、独立RP-PPPoE3.15/pppd2.4.9・
  ISC DHCP4.4.3-P1・別Linux LAN端末で、CHAP、PD/Renew、両family TCP/UDP、
  学習DNSのUDP/TCP、古い通知409、元期限の失効、server復旧、5秒confirmed rollback、
  HTTP200のPD単独/PPP同時削除、PPPのMTU/MRU変更が通った。
  service再起動に加え、control再起動で旧PDをReleaseし、新control・PPP・PDで転送とDNSが復旧した。
  network/managementのPID、設定revision、DUIDを維持し、旧PD producerの通知は409で拒否した。
  WAN/LAN captureで寿命0 RA→Release→対応Reply→PADTを確認し、PID1停止も完了した。
- APK由来Linux6.18.58-0-lts/amd64を実PID1で起動し、virtio WAN/LAN・独立AC/ISC・
  別Linux SLAAC端末、通常DADで、service/control両方の再起動と再取得後のTCP/UDP/DNSが通った。
  新規256MiBのregular-file ext4 diskだけをfixtureで作り、`state_volume=/dev/vda`を明示した。
  CLI poweroffは同じ撤去順序、kernel Power downとQEMU exit0を確認した。
- 別起動ではLAN MTU1400/revision2をcommitし、取得・適用完了と転送を確認してから実rebootした。
  最終peer出力で撤去順序と、同じVMの二回目kernel/PID1でのrevision・MTU・DUID・
  固定SSHホスト鍵の維持、新しいPPP/PDによる両family TCP/UDP/DNS復旧を確認した。
  この再開時には終了済みPTYのexit codeを取得できなかったため、終了コードの再確認とは区別する。
- `make check build-linux RELEASE_ID=control-drain`成功。全Go/Python、Race Detector、vetと
  amd64/arm64の6静的ELFを含む。`make image DOCKER='sudo -n docker' RELEASE_ID=control-drain`も成功。
  arm64は実行していない。最終6 ELFの署名catalog/publisher/stage検査もRace Detector付きで成功した。
- `make check-health-pbr RELEASE_ID=control-drain`成功。実supervisor/control/network/Healthと独立2 WANで、
  TCP/UDPの切替・復旧、Health/control/network再作成、古い通知・元期限/利用側鮮度の失効、
  writer再起動による失効後の撤去、confirmed rollbackをcaptureと利用者応答で確認した。

最初の修正後appliance試験はpeer側ISCのbind前提で失敗した。新PPP processのReadyだけでは
AC側interfaceの作成完了を表さないため、既存待ちの中でAC側ifindexの交代を確認してからbindする。
その修正後のpoweroffとreboot試験で実通信を確認した。通信失敗のretryや期限延長では代用していない。

SIGKILLで旧controlを失った場合のgraceful Releaseは保証していない。
control再起動中の無応答peer、更新trial・実suspend・物理機の電源動作も未検証。
過去のHTTP422を無視したPPP/PD同時削除の成功判定は取り下げ済み。現在はHTTP200と実通信を確認する。

## 再実行と環境

`umask 022`でproject Makefileの静的build/image経路を使う。現在の独立ISC binaryは
`dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`。実通信は重いbuildと分けて実行する。

- `python3 scripts/pppoe-peer.py --auth chap --dhcp6-server <ISC-dhcpd>`
- `make qemu-pppoe DHCP6_SERVER=<ISC-dhcpd>`（poweroff）
- `make qemu-pppoe DHCP6_SERVER=<ISC-dhcpd> QEMU_POWER_ACTION=reboot`（永続stateと二回目起動）
- `make check-dual-wan DOCKER='sudo -n docker' RELEASE_ID=<release>`
- `make check-dhcp6 RELEASE_ID=<release>`
- 実上流は`make check-uplink UPLINK_INTERFACE=<authorized-interface> UPLINK_AFTR=<provider-name>`。
  専用MACのmacvlanと別LAN namespaceを使い、管理address/route/clockを変更しない。

rootで動かすharness/binaryはroot所有0700一時ディレクトリへ配置する。
namespace・daemon・captureの所有者は時間上限と終了時cleanupを持つ。homeの権限を緩めない。
PPP engineは固定Alpine3.24.3のpppd/pppoe2.5.3-r0、musl・libpcap・OpenSSL provider閉包を配る。
署名manifestでengine/sourceを結び、supervisorはFD5を固定する。private chrootから
host engine・設定・hookへfallbackしない。対応source/APKBUILD/patchとlicenseは保持する。

## 実上流で確認した運用条件

- Replyの送信元49153を受け入れる。通常のリンクローカルDADを待ち、広告には現在のMACを使う。
- PPPでは永続DUID-UUIDか明示DUIDを使う。元のboot期限を受信時刻へ付け直さない。
- DS-LiteはDHCPv4 defaultを無効にし、選択FIBとAFTRのカプセル化往復まで確認する。
- native-27の別cohortは約130分・756回の定期両family HTTPS/UDP・TCP DNSを終了コード0で完了した。
  自然T1のRenew/Reply（送信元49153）と期限更新、全worker世代1を確認した。
  最新native-28の長時間受入、同一TCP継続、ONU直結配線や物理NICの資格判定には数えない。

## 優先順と残作業

全work packageは台帳の範囲を維持する。今回の相互接続だけで全体を完了にしない。
実行順序はhomert02/03の現行運用代替、次に実態を調べたk8s-rt/pve-rt、その後既存文書に基づくCloud SAM。
直近はhomertの実設定・条件付き稼働状態を確認し、HA-01/02/03とEXT-02のうち、
実際に使う選出・DHCP/PD同期・接続状態同期・Tailscaleを完成させる。本番切替は行わない。

- 全kernel-link再設定の依存順序、control強制終了時の復旧、
  実suspend/更新trial中の電源操作・無応答peer時のcontrol交換と全機停止、VRF、複数IAの
  DAD共存、resource readiness/RA連携、全link optionとWAN選択。
- DHCPv6 server、relayのHA/動的interface・VRF、動的pool/options/DNS、lease import。
  DHCP server journal等のUTC依存と、実clock補正・停電・suspend時の期限。
- BGP/BFDの全resource/profile、大規模RIB、IPv6 single-hop VRF・link-local next-hop・ECMP。
  過去のBFD再接続timeout・IPv6 multicast未受信・初回VIP TCP/DAD timeoutは原因未確定。
- VRRP/cluster、認証付きDHCP/PD同期、NAT session継続、SAM/federation/cloud、全運用surface、
  外部engine、権限分離、OS A/B・更新/復旧・source配布。factory kernel対応source archiveは未同梱。
- arm64実行、container PPP、実ISP PPP、対象物理機・NIC・性能・電源断と直接ONU配線の受入。
  対象実機条件は未確定。独立して進められる実装・隔離VM試験を継続する。

完了したlog/capture・一時harnessは削除し、結果・手順と未検証範囲だけを残す。
過去の診断出力を再保存しない。配布に必要なsource・licenseは削除対象にしない。
