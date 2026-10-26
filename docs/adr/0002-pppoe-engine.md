# ADR 0002: PPPoE は既存の foreground pppd 経路を統合する

日付: 2026-10-26。状態: worker と隔離 Linux profile を実装、配布 engine は未完了。

routerd の `pkg/pppoeclient` は PPP を独自実装せず、`pppd` と RP-PPPoE を使っている。
routerd2 でも、この実装の `LinuxPeer` を土台にした設定生成を使う。未実装の native PPP engine を
新規作成することは、相互接続を早く成立させる今回の目的に必要ない。

- supervisor が PPPoE worker を所有し、worker が foreground pppd を一つ所有する。
  child の終了を待ってから worker が終了する。強制終了時は Linux の親終了通知で
  child も終了する。作成した OS thread は child の終了まで保持する。
  setuid pppd への exec でも通知が消えないよう `NO_NEW_PRIVS` を設定する。
- pppd が PPP unit と IPCP/IPv6CP の交渉・アドレスを所有する。network writer は
  そのアドレスを再設定せず、有限の観測に基づく経路・転送・NAT を所有する。
  LCP と再接続は pppd と既存 supervisor のそれぞれの寿命に属する。
- 接続済みの判定はログ中の文字列ではなく、実際の kernel の local/peer address を使う。
  観測が更新されなければ、writer は元の BOOTTIME 期限で経路を撤去する。
- 資格情報は明示した private password file から読む。argv と観測へ password を入れない。
  child 専用 mount namespace の `/etc/ppp` と `/run` に設定・DNS・PID file を閉じ込める。
  pppd が読む root の `.ppprc` も隠し、ホストの既存設定や shell hook を実行しない。
  `usepeerdns` の結果は child の private file から型付き観測へ渡す。

PVE Linux 6.8 / amd64、pppd 2.4.9 と RP-PPPoE 3.15 の独立 AC で、PAP/CHAP、
MTU 1454、DNS、LAN TCP/UDP、IPv6CP の link-local ping、worker の強制終了、
元の観測期限、古い世代の通知、認証拒否・修復、AC 停止・復旧を実通信した。
再実行は `make check-pppoe`。通常の DAD を無効にしない。

Alpine 3.24.3 / pppd 2.5.3-r0 の実 rootfs でも同じ PAP/CHAP・転送・障害復旧を確認した。
旧 alias `rp-pppoe.so` はこの配布物に存在しないため、両 OS が持つ `pppoe.so` を使う。
DNS 出力先は child 内の `/run/ppp/resolv.conf` に一本化し、pppd 2.4 の出力 path を
同じファイルへ向ける。root の `.ppprc` に別 MTU がある fixture でも設定した 1454 を保つ。
LCP capture でも client の MRU 1454 と peer の ACK を確認した。

別 OS の確認は `make pppoe-peer-profile DOCKER='sudo -n docker'` で固定版 rootfs を出力し、
`python3 scripts/pppoe-peer.py --engine-archive dist/pppoe-peer-profile.tar` で再実行する。
この archive は試験用であり、署名 runtime や appliance の受入ではない。

この Linux profile は `/usr/sbin/pppd`、対応する `pppoe.so`、`/dev/ppp` と kernel の
PPP/PPPoE 機能を必要とする。現時点の scratch runtime / appliance に engine を同梱した
とは扱わない。配布する正確な engine・plugin・loader/library・license の閉包と署名契約は
CORE-06 / NET-04 の残作業である。Go の静的 build 成功で代用しない。

PPP 上の DHCPv6 PD、resolver への DNS 適用、完全な旧 resource field の意味、
DS-Lite との二経路 Health/PBR・MSS、実 ISP と物理 NIC の受入も未完了である。
これらの結果に応じて今回の interface・配置・契約を変更する。prototype 互換層は作らない。

PPP の有限 `.active` fact と明示した依存関係で Health を起動する。PPP のアドレスと
UP 状態は引き続き pppd が所有する。専用 routing table と marked probe を使う場合、
Linux の RPF が応答を拒否し得る。旧 routerd の router profile と同様の効果は、明示した
`forwarding.ipv4_rp_filter: 0` で選ぶ。all/default の元の値は journal が所有し、設定撤去で
復元する。既存 interface の個別 RPF 値は変更しない。実 peer で Health→PBR→TCP/UDP と
設定撤去・confirmed rollback を確認した。DS-Lite との二経路受入は依然として残る。

参照:

- [再利用した routerd の設定生成](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/pppoeclient/client.go)
- [pppd 2.4.9 の user option 読み込み](https://github.com/ppp-project/ppp/blob/ppp-2.4.9/pppd/options.c)
