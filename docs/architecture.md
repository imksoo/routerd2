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

core 4 プロセスに加え、DHCP4 client/server、PD、RA、DNS、VRRP、BGP をインスタンスごとの
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
ACK は kernel projection の適用完了後に返す。変更途中の失敗時は前の設定を再投影する。
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
ディスクの A/B bootloader、署名付き update engine、永続 partition の自動作成、
AMI/OCI/Azure/GCP image の登録は未実装。Tailscale/strongSwan 等の外部 daemon もまだ
起動できると偽装しない。暗号・IKE・overlay controller を新規自作して機能の存在だけを
合わせる方針にはせず、module contract とパッケージ/license の単位を個別に実装する。
