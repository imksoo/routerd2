# routerd2 の責務とプロセス構成

## Linux と routerd の境界

appliance は Linux の起動後に `/init` から routerd を PID 1 として開始する。
routerd は proc/sys/dev/run の初期化、必要な kernel module の syscall によるロード、
子プロセスの起動・回収・監視、停止を行う。systemd、journald、OpenRC、sshd は実行しない。
container/host ではプラットフォームを明示し、appliance 用 mount や kernel module
ロードを実行しない。コンテナの PID 1 であることだけを appliance と判定しない。

IP の転送は Linux、ルーティングプロトコルと設定制御は routerd の責務である。
Alpine は kernel/firmware/パッケージと image assembly の供給元として利用する。
apk/OpenRC をパケットの処理経路や daemon の起動管理に挟まない。

## 2 つの ELF と複数の障害単位

- `routerd`: init/supervisor と運用 CLI。構成をまとめて読み、固定の worker ELF を開く。
- `routerd-modules`: logging/network/control/management と protocol worker を選択起動する。

core 4 プロセスに加え、DHCP4 client/server、PD、RA server/client、AFTR、DNS、VRRP、BFD、BGP をインスタンスごとの
プロセスとして追加する。GoBGP/QUIC/SQLite は worker ELF に置き、PID 1 の import
graph へ入れない。ELF の数は fault isolation の単位ではない。同じ ELF の読み取り専用
text page は Linux が共有でき、プロセスごとの heap・goroutine・socket は独立する。

supervisor は起動時に worker ELF の FD を保持する。起動後の symlink 変更やファイルの
置換で別 release の worker を同じ process tree に混ぜない。worker はビルドに埋め込まれた
release ID を専用 FD で通知し、PID/role/name/sequence と heartbeat を検査する。
FD の閉鎖、startup/heartbeat timeout、exit は状態に残し、役割ごとに再起動する。

PID 1 に通常のプロトコル socket や DNS/BGP engine を置かない。初期版では worker は
同じ UID で動く。FD と private UDS による分離は ABI とメンテナンス上の境界であり、
すべての worker を相互に不信任とする権限 sandbox の完成を意味しない。

## 設定と動的な状態

`config.Document` は永続する意図であり、DHCP の取得アドレスを直接書き戻す場所ではない。
control は設定と protocol facts から完全な `network.Desired` を生成する。
network だけが netlink/sysctl/nftables を変更する。protocol worker は native socket を
所有し、設定 identity と世代・epoch・sequence を付けた typed snapshot を送る。

設定にある任意 executable/argv を supervisor に渡す経路はない。dynamic module role は
許可リスト、instance は固定ディレクトリの SHA-256 content-addressed JSON である。
worker 自身が exact bytes を検証する。設定由来の journal/socket パスも compiler が割り当てる。

変更した worker は停止・reap を待って置換する。変更していない protocol config の取得状態は
維持する。停止中に DHCP が更新した場合も、cutover 時点で最後に ACK したリースを採用する。
アドレス・経路などの所有権を更新する ACK は kernel projection の適用完了後に返す。
BFD の受信観測は専用の状態経路で受理し、大きな kernel projection の lock や fsync を待たない。
変更途中の失敗時は前の設定を再投影する。
nftables batch は atomic だが netlink/sysctl/プロセス全体は一つの atomic transaction ではない。

control crash の復旧は前世代の dynamic worker を一度停止してから再構成する。
この時の WAN/HA reacquisition は hitless ではない。設定 journal、SSH host key、protocol
identity を再利用できるよう state directory を永続化する。

## リース・HA・prefix 更新

DHCP の有効期限を network address lifetime にも設定し、controller が停止しても Linux
が期限を過ぎたアドレスを残し続けないようにする。PD 更新では current と still-valid previous
prefix を保持し、旧アドレスは preferred=0 とする。RA worker の identity は LAN template
から作り、更新は同じ socket に通知する。更新のたびに router lifetime=0 を送らない。

VRRP は IPv4/IPv6 ごとの v3 election を行う。Active への移行では VMAC macvlan と VIP、
必要な Accept_Mode guard の適用 ACK 後に実物を検査してから広告する。VIP の DAD 無効化は
HA VIP ごとの明示設定であり、インターフェース全体の DAD を無効にしない。
Active worker は広告ごとに有限の所有権を更新する。controller は wall time に加えて
受信時の monotonic deadline を持ち、clock の巻き戻りで所有権を延長しない。
SIGKILL や停止した worker の replay だけで Active を維持しない。

BGP の選択経路は full RIB snapshot から network に投影する。ECMP の複数経路を
単一 RouteReplace の繰り返しで潰さない。next hop の再帰解決は committed connected/static/
learned intent と VRF table に従う。未解決 path は `show services` の `route_issues` に
表示し、他の有効な経路や BGP session は維持する。kernel 適用エラーは NACK とする。
worker の snapshot transport が bounded JSON のため、bundled BGP は現状 4096 selected
paths までを受理する。100 万経路や無停止 FIB 更新を検証済みとは扱わない。

BFD は packet receive の `CLOCK_BOOTTIME` と boot ID で有限の正常判定を行う。
各 BGP peer は明示した BFD service に依存し、startup も含めて正常通知がある場合だけ
接続を許す。Down counter と source identity は Up→Down→Up をまとめた通知でも保持する。
GoBGP の disable/enable 呼び出しは非同期なので、実際の Down/Idle を確認し、旧経路の
撤去 ACK と新しい受信証跡を得てから再接続する。RIB walk 前後の gate 世代も照合し、
古い経路に新しいセッションの証跡を付けない。詳細と制約は [BFD 契約](bfd.md) に記す。

## WAN RA と AFTR

WAN RA は kernel RA/autoconf を制御して一つのアドレス・経路所有者を維持し、
有限 lifetime を kernel address/IPv6 route にも反映する。DAD は通常どおり Linux が実行する。
RA の prefix、default/RIO、DNS はそれぞれ独立した期限を持つ。

AFTR worker は明示された RA/DHCPv6 の typed input だけを受け取り、IPv6 underlay に
source/device を固定して DNS を問い合わせる。DHCPv6 の情報更新期限、source preferred/valid
lifetime、DNS chain TTL を超えて古い endpoint を残さない。config digest と dependency
generation で古い応答を拒否し、kernel の投影だけを network worker に依頼する。
詳細な意味と制約は [WAN 契約](wan.md) に記す。

## 設定編集と rollback

SSH は x/crypto/ssh の公開鍵認証と routerd CLI だけを提供する。OS shell、forwarding、
SFTP を許可する汎用 SSH daemon ではない。candidate は session ごとに分ける。
`set/delete/edit/diff/validate` は候補を操作し、`commit` が revision と request ID を添えて
control API に提出する。他セッションの変更は expected revision の不一致で検出する。

`commit confirmed N` は前設定と deadline を同じ durable record に保存する。
SSH 切断に依存せず control がタイマーで復元する。未確認状態の control 再起動でも復元する。
`confirm` は現在の revision/request ID、projection、required worker readiness を確認する。
復元に失敗した場合は degraded を残し、成功したことにしない。
YAML 文書編集は全置換・検証付き paste editor であり、Vim 互換の full-screen editor ではない。

health には設定全体や各 protocol snapshot を含めない。大きな confirmed commit の
成功応答は、revision・request ID・candidate digest を含む bounded ACK に切り替える。
設定全体の読取は metadata と revision を固定した document API で行う。network の
summary と詳細ページは省略・件数を明示し、AFTR の source address 依存は別の小さい
native query で取得する。大量の経路や WireGuard peers がこの依存の応答サイズを
押し上げることはない。[API の契約](../internal/control/README.md) を参照。

## DHCP と DNS の連携

DHCP server はリースを fsync し、型付き snapshot に対する coordinator の ACK を受けてから、
クライアントへ ACK を返す。DNS は committed config が許可した DHCP source だけを受け取る。
リース内容の更新は zone table を置き換え、listener や既存 TCP connection を再起動しない。
静的レコードを優先し、A/PTR の TTL はリース寿命と producer の有限 ownership の短い方で制限する。
DHCP producer を SIGSTOP した場合にも動的レコードは失効し、静的 DNS は継続する。
heartbeat だけの更新では upstream cache を全消去しない。

## journald を外した後のログ

logging worker は bounded memory tail、private log API、NDJSON の回転ファイル、
restart recovery と producer/server epoch を管理する。worker からの送信 queue は bounded
で、logging の停止・disk error・遅い console writer が caller を無期限に止めない。
欠落・fallback・再接続はカウンタと gap として表示する。

appliance は `/dev/kmsg` から kernel log を収集する。これは stdout/stderr の全 FD を
pipe で吸い込む仕組みではなく、structured slog 以外の panic/stdio は console に出る。
HTTP の ACK は memory への受信であり、すべての行を fsync 済みと保証する audit log ではない。
ディスク同期は周期処理、問い合わせは bounded tail であり、全履歴の全文検索ではない。

## 配布と実装済み境界

2 バイナリは同じ release ID で build/package する。単一 ELF に全コードを押し込むことより、
同じ release の構成物・設定 schema・module protocol が揃うことを保証対象にする。
APK、OCI、initramfs に依存ライセンスと provenance manifest を同梱する。

現在の appliance artifact は kernel と initramfs の direct boot fixture である。
ディスクの A/B bootloader、永続 partition の自動作成、
AMI/OCI/Azure/GCP image の登録は未実装。Tailscale/strongSwan 等の外部 daemon もまだ
起動できると偽装しない。暗号・IKE・overlay controller を新規自作して機能の存在だけを
合わせる方針にはせず、module contract とパッケージ/license の単位を個別に実装する。

## 署名付き userland の世代選択

`internal/release` は Ed25519 manifest と両 ELF のサイズ・hash・architecture・static build・
一意な release identity を検証して保存する。新しいバイナリを検証のために実行しない。
active/pending/previous と状態 journal を同じ generation に置き、current symlink の atomic
rename で選択を変える。stage と prepare は動作中の process tree を置換しない。

appliance の `boot.release` が有効な場合、同じ kernel boot ID の選択を一度だけ記録し、
検証済み routerd の FD から exec する。PID 1 は変わらず、選択された modules FD も supervisor
が pin する。試行世代の必須 process の release/generation、control の revision/health が
連続して安定した後だけ Confirm する。期限超過は失敗記録・旧世代選択・再起動につながる。
監視開始前の PID 1 hang や kernel failure を検出・reset する hardware watchdog は別途必要。
ここでの readiness は process と設定投影の健全性であり、ISP への疎通や外部サービスの
到達性までは検証しない。プロトコルの依存待ちと、起動できないプロセスは区別する。

これは署名付き userland pair の切り替えである。kernel/rootfs/bootloader の更新、rollback
可能な設定 schema migration、永続 disk の初期化、SSH 経由の bundle upload は含まない。
state が initramfs の RAM にしかない場合、選択 journal も次の電源投入には残らない。
release と config/host-key の保存先を永続 filesystem に配置する責任は image 側にある。
