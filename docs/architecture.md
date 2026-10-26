# routerd2 の責務とプロセス構成

この文書は現在の未リリース実装を説明する。今後は、単一配布イメージから filesystem へ展開した
多 binary の runtime と、小さな boot 層、startup-config を分離する。
実行ファイル数を二つに固定せず、試作形式との後方互換も要求しない。採用した設計と置換方針は
[ADR 0001](adr/0001-boot-runtime-startup-config.md) を参照。

## Linux と routerd の境界

appliance は Linux の起動後に `/init` から routerd を PID 1 として開始する。
routerd は proc/sys/dev/run の初期化、必要な kernel module の syscall によるロード、
子プロセスの起動・回収・監視、停止を行う。systemd、journald、OpenRC、sshd は実行しない。
container/host ではプラットフォームを明示し、appliance 用 mount や kernel module
ロードを実行しない。コンテナの PID 1 であることだけを appliance と判定しない。

IP の転送は Linux、ルーティングプロトコルと設定制御は routerd の責務である。
Alpine は kernel/firmware/パッケージと image assembly の供給元として利用する。
apk/OpenRC をパケットの処理経路や daemon の起動管理に挟まない。

## 現在の実装: 2 つの ELF と複数の障害単位

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

`config.Document` は [ResourceProgram](resource-program.md) の source で、ID・依存・owner・provenance を永続する。
compiler は source を変更せず、型付きの内部 `Native` と graph/ownership の plan を作る。
DHCP の取得アドレスを source に直接書き戻さず、control は compiled intent と protocol facts から完全な `network.Desired` を生成する。
network だけが netlink/sysctl/nftables を変更する。protocol worker は native socket を
所有し、設定 identity と世代・epoch・sequence を付けた typed snapshot を送る。

設定にある任意 executable/argv を supervisor に渡す経路はない。dynamic module role は
許可リスト、instance は固定ディレクトリの SHA-256 content-addressed JSON である。
worker 自身が exact bytes を検証する。digest計算と保存には同じ準備済みbytesを使う。
この設定・bytes・digestは条件でまだ起動していないworkerも含めてsource切替時に一度生成する。
通常の観測投影は不変のworker集合から起動対象を選ぶ。prefix/DNSを別途受け取る生成RAも同じ契約を使う。
instanceファイルとBFD設定は観測用mutexの外で準備し、worker停止待ち後には最新の観測を
再投影する。instance構成が変わった場合だけ準備・停止をやり直す。
設定由来の journal/socket パスも compiler が割り当てる。
protocol投影は変更するnetwork集合を複製し、固定設定を読み取り専用で共有する。
元の宣言を変えずに学習情報と期限切れを反映し、JSON往復による複製は行わない。

変更した worker は停止・reap を待って置換する。変更していない protocol config の取得状態は
維持する。停止中に DHCP が更新した場合も、cutover 時点で最後に受理したリースを採用する。
`POST /v1/services/state` は観測の検証・受理を返し、kernel適用はcoordinatorが行う。
BGPとVRRPはプロトコルを次へ進める前に実適用が必要なため、
`POST /v1/services/effects` の適用完了を待つ。起動・停止待ち中も同じ適用処理を進め、
初回Readyや最終withdrawalと循環させない。条件の有無で通知の意味を変えない。
通常監視・設定切替・起動停止待ちは同じ一回分のreconcileを進める。supervisorの実登録を
使い、別のinstalled集合は保持しない。通常監視は停止要求後に戻り、次回に実際の終了を
確認する。設定切替だけが次設定に残すworker集合を上限として期限付きで待つ。
切替前は退役workerの実終了を待ち、継続workerのReadyは新設定の適用後に確認する。
復旧に必要な新設定を、現在の準備失敗が妨げないようにする。
設定から外すworkerの終了前に前提を撤去しない。自分のinactive適用を待つBGP/VRRPは、
他の退役workerが終了した後に撤去ACKを返す。継続するworkerのstopping/failedは、
経路の期限切れや独立した観測の適用を止める理由にしない。
BGP経路は元のboot IDとBOOTTIME期限をnetwork workerの所有記録へ渡す。
network workerはcontrolと独立して期限切れ経路を撤去する。期限だけの更新では
kernel経路を置換しない。writer自身が停止した間のIPv4失効は保証しない。
受理済み観測をkernel障害で巻き戻さず、coordinatorが最新の観測を再適用する。
kernel I/O中は観測用mutexを解放する。適用開始後の新しい観測は、その古い結果で
完了扱いにしない。未適用の観測や適用中の処理があればReady/confirmed commitを拒否する。
BFDの受信観測は専用の状態経路で受理する。設定変更の失敗時は前の設定を再投影する。
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

graceful activationはcoordinatorが適用開始時に有限の準備条件を評価する。
同じ適用要求の応答でVIPの実所有状態を返し、grant往復・仮VIP・期限昇格は行わない。
成功した選挙のkeyだけを保持し、その後の準備喪失では撤去しない。適用失敗は入場許可を
残さず、再試行で現在の準備条件を使う。VRRPの鮮度と条件factは元の選挙観測を直接使い、
受信時刻から作る別リースや、失効を表すJSON書換えを置かない。

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
