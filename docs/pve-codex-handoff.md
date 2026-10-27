# routerd2 開発引き継ぎ

2026-10-27。全実装計画のゴールは継続中。対象実機で利用可能とは判断していない。
機能範囲・未完了項目は[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)に従う。[AGENTS.md](../AGENTS.md)を優先する。
過去のAPI・保存形式・試験結果を維持するための実装は残さない。

## 現在の変更: appliance-power / native-28

local/SSH CLIの`poweroff`と`reboot`、localの`routerd poweroff/reboot`を追加した。
private supervisor APIへ型付きの操作を送り、appliance PID1だけが受け付ける。
受付応答のflush後、更新候補の正常性監視を止め、既存のworker停止順序で終了・reapする。
entrypointへ通常の電源操作結果を返し、sync後にLinuxのpoweroff/reboot syscallを実行する。
host/containerモードは拒否する。再起動要求をruntimeの失敗errorとして表さない。
更新候補は、意図した停止だけでは確認済みにしない。

`restart service.NAME`もcontrolへ送り、設定変更と同じ静的依存グラフ・coordinatorで
子を先に停止・reapしてから現在の設定で起動し直す。直接supervisor経由のdynamic service再起動は拒否する。
202は古いworkerの停止と起動再要求の受付であり、PPP/PDの取得完了ではない。
設定revisionと無関係なDNS workerを維持する。旧routerdの`pkg/pdclient.Client.Release/send`を比較した。
boot ABI1・worker ABI4・manifest v4・network journal9・compiler native-28は維持した。

## 確認した範囲

最終`appliance-power`の実装で確認済み:

- PVE Ubuntu Linux6.8/amd64、署名PID1 catalog、独立RP-PPPoE3.15/pppd2.4.9・
  ISC DHCP4.4.3-P1・別Linux LAN端末で、CHAP、両family TCP/UDP、学習DNSのUDP/TCP、
  PD/Renew、worker再作成・同じDUIDでの再取得、古い通知409、元期限の失効、server復旧、
  5秒confirmed rollback、HTTP200のPD単独/PPP同時削除、PPPのMTU/MRU変更が通った。
  control経由のoperator再起動はRelease→Reply→PADTとなり、通信・DNSが復旧した。
  revision/DNS PIDを維持し、直接supervisor操作は拒否された。local CLIのpoweroffも202で受け、
  寿命0 RA→Release/対応Reply→PADT後、Linux PID namespaceの電源断で終了した。
- APK由来Linux6.18.58-0-lts/amd64の最終kernel/initramfsを実PID1で起動した。
  virtio WAN/LAN・独立RP-PPPoE/ISC・別Linux SLAAC端末、通常DADで、CHAP、DNS、
  両family TCP/UDP、PD/Renew、CLIのservice再起動と再取得後の通信が通った。
  256MiBの新規regular-file ext4 diskだけをfixtureで作り、`state_volume=/dev/vda`を明示した。
  CLI poweroffの寿命0 RA→Release→対応Reply→PADT、kernel Power down、QEMU exit0を確認した。
- 別起動ではCLIでLAN MTU1400をcommitし、protocol取得・適用完了と通信を確認してから実rebootした。
  同じ撤去順序の後、同じVMが二回目のkernel/PID1を起動した。新しいsupervisor instanceで
  revision 2・MTU1400・DUIDと固定したSSHホスト鍵を維持し、新しいPPP/PDで両family TCP/UDP・
  UDP/TCP DNSが復旧した。最後のCLI poweroffもkernel Power downとQEMU exit0で終了した。
  host block deviceは使っていない。物理機・更新候補trial・電源断fallbackの受入ではない。
- hostモードの実UDSは両電源要求を拒否し、同じworker PIDでReadyを維持した。
- `make check build-linux RELEASE_ID=appliance-power`と先行`make build build-publisher`成功。
  全Go/Python・Race Detector・vet、amd64/arm64の6静的ELFを含む。arm64は実行していない。
  最終6 ELFの署名catalog・publisher・stage検査もRace Detector付きで成功した。

最初の永続state試験では、PPP service再起動が40秒でtimeoutした。原因未確定。
同じ実装の後続実行は通ったが、先行失敗を解消済みとは数えない。失敗時のstatus/config/services/logを
取得する。MTU commit直後のcurl失敗ではPDが再取得中でconfiguration phaseもapplyingだった。
fixtureはprocess Readyだけで通信を始めず、現在のPDと適用完了を待つ。通信失敗のretryで代用しない。
以前のビルド並行実行の3秒API timeoutも、高負荷時の応答時間の合格には数えない。
HTTP422を無視した過去の誤判定は修正済みであり、成功HTTPと実通信を必ず確認する。

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

## 残作業

全work packageは台帳の範囲を維持する。今回の相互接続だけで全体を完了にしない。

- 原因未確定の永続state PPP再起動timeout、全kernel-link再設定の依存順序、control再起動中の解放、
  実suspend/更新trial中の電源操作・無応答peer時の全機停止、VRF、複数IAの
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
