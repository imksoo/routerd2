# routerd2 開発引き継ぎ

2026-10-27。全実装計画のゴールは継続中。対象実機で利用可能とは判断していない。
機能範囲・未完了項目は[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)に従う。[AGENTS.md](../AGENTS.md)を優先する。
過去のAPI・保存形式・試験結果を維持するための実装は残さない。

## 現在の変更: service-restart / native-28

`restart service.NAME`をcontrolへ送り、設定変更と同じ静的依存グラフ・coordinatorで
子を先に停止・reapしてから現在の設定で起動し直す。supervisorへのdynamic serviceの
直接再起動は拒否する。新しいregistry・producer寿命の模擬実装・互換APIは追加していない。
202の`accepted`は古いworkerの停止と起動再要求の受付であり、PPP/PDの取得完了ではない。
設定revisionを変更せず、関係のないDNS workerを維持する。

修正前の実applianceでは、`restart service.internet`がPDのRelease前にPPPを切断した。
独立ISCサーバーを動かしたまま再起動する試験で再現し、修正後に同じ経路で確認した。
旧routerdの`pkg/pdclient.Client.Release/send`も比較元である。
boot ABI1・worker ABI4・manifest v4・network journal9・compiler native-28は維持した。

確認済み（最終`service-restart`ビルド）:

- PVE Ubuntu Linux6.8/amd64、署名PID1 catalog、独立RP-PPPoE3.15/pppd2.4.9・
  ISC DHCP4.4.3-P1・別Linux LAN端末で、CHAP、両family TCP/UDP、学習DNSのUDP/TCP、
  PD/Renew、worker再作成・同じDUIDでの再取得、古い通知409、元期限の失効、server復旧、
  5秒confirmed rollback、HTTP200のPD単独/PPP同時削除、PPPのMTU/MRU変更が通った。
  control経由のoperator再起動もRelease→Reply→PADTとなり、通信・DNSが復旧した。
  設定revision/DNS PIDを維持し、直接supervisor操作は拒否され、PPP PIDは不変だった。
  PID1正常停止でも寿命0 RA→Release→対応Replyを確認した。
- APK由来Linux6.18.58-0-lts/amd64の最終kernel/initramfsを実PID1で起動した。
  virtio WAN/LAN・独立RP-PPPoE/ISC・別Linux SLAAC端末で、CHAP、学習DNS、
  両family TCP/UDP、PD/Renew、CLI再起動と再取得後の通信が通った。
  WAN/LAN captureで寿命0 RA→Release→同じtransactionのReply→PADTを確認した。
  manifest/worker release ID、DUID、設定revisionとDNS worker世代も確認した。
  QEMUを終了するfixtureであり、applianceの正常poweroffの受入ではない。
- `make check build build-publisher build-linux RELEASE_ID=service-restart`成功。
  全Go/Python、Race Detector、vet、amd64/arm64の6静的ELFを含む。
  最終6 ELFの署名catalog・publisher・stage検査もRace Detector付きで成功した。arm64は実行していない。

先行の負荷並行実行は3秒の設定API待ちでtimeoutしたため合格に含めない。
最終版をビルド負荷なしで再実行して上記を確認したが、高負荷時の応答時間は未受入である。
以前のPPP/PD同時削除の誤判定（HTTP422の無視）は修正済み。HTTP成功を必ず確認する。

## 再実行と環境

`umask 022`でproject Makefileの静的build/image経路を使う。現在の独立ISC binaryは
`dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`。実通信は重いbuildと分けて実行する。

- `python3 scripts/pppoe-peer.py --auth chap --dhcp6-server <ISC-dhcpd>`
- `make qemu-pppoe DHCP6_SERVER=<ISC-dhcpd>`
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

- 全kernel-link再設定の依存順序、control再起動中の解放、実suspend/reboot、VRF、複数IAの
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
