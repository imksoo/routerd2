# 初回 state 移行手順

この手順は homert02/03 を routerd2 の新規 BIOS disk へ一台ずつ移すときの境界を定める。
旧 routerd の state directory を丸ごとコピーしない。旧 daemon と新しい PID 1 が同じ kernel
資源、DHCP 配布、Tailscale identity を同時に所有しないことを前提にする。

## 入力と所有者

|入力|取得元|新しい配置|停止が必要な境界|扱い|
|---|---|---|---|---|
|Tailscale saved identity|旧機の`/var/lib/tailscale/tailscaled.state`|`/var/lib/routerd/services/tailscale/tailscaled.state`|旧`tailscaled`停止、process・TUN・table 52 の消失確認後|opaque regular fileをそのまま一度だけ複写する|
|SSH host key|旧機の選択済み Ed25519 host key|`/var/lib/routerd/ssh_host_ed25519_key`|管理接続の切替時|既存の管理 host identity を保持する必要がある場合だけ複写する|
|DHCPv4 lease|停止済み旧 dnsmasq lease file|新 disk の native DHCP service state|旧 DHCP server 停止後|`routerd dhcp import-leases` で取り込む|
|DHCP sticky history|停止済み旧 routerd SQLite DB と`-wal`/`-shm`|新 disk の native DHCP service state|旧 DHCP server 停止後|lease import 成功後に`routerd dhcp import-sticky`で取り込む|
|router YAML|現行 operator intent|新 image の`/etc/routerd/config.json`|停止不要|変換・validate 済み設定を image build にだけ渡す|

旧 routerd の lease/state/database、Tailscale state directory 全体、鍵のない EnvironmentFile、
runtime socket、kernel state は入力にしない。opaque state の内容、鍵、SHA-256 値は記録・出力・
commit しない。

## 一台の実行

1. 新しい disk、native config、回復用 console と旧機を再起動できる管理経路を準備する。新 disk は
   新規 output にだけ作る。既存 disk を上書きする builder は使わない。
2. 旧機をまだ動かしたまま、設定だけを`routerd config migrate`と`routerd config validate`に通す。
   Tailscale state、lease、sticky DB をこの段階で読んだりコピーしたりしない。
3. 承認された停止時だけ、旧 DHCP/Tailscale を止め、旧 process、`tailscale0`、Tailscale table 52、
   UDP 41641 の旧 owner と DHCP listener が消えたことを確認する。消えない場合は新 disk を起動しない。
4. root 専用の空 stage を作り、表の選択済み regular file だけをコピーする。directory は0700、file は0600
   とし、copy 前後の SHA-256 をその場で比較するが値を端末出力・文書・履歴へ残さない。symbolic link と
   device は受け入れない。
5. `DISK_STATE_DIR` にその stage だけを渡して新 disk を作る。builder は新規 ext4 state partition にだけ
   copy し、`services/tailscale` を Tailscale engine の state directory として bind する。lease と
   sticky は新 disk の停止状態で、lease import を先、sticky import を後に実行する。
6. 起動直前に stage の mode/type と複写の SHA-256 一致、native config validation、disk manifest の
   `initial_state_provisioned`、旧 owner 不在を確認する。新しい PID 1 を一台だけ起動し、独立 peer から
   Tailscale node identity、IPv4/IPv6 TCP/UDP、DHCP renewal、新規 DHCP client、DNS、WAN/LAN 通信を確認する。
7. 失敗したら新 PID 1 を止めてから旧 service を起動する。旧 state の再 SHA-256 が停止時の値と一致することを
   その場で確認し、専用 disk と stage を削除する。二つの owner を同時に起動して復旧しない。

各 node は同じ手順を独立に実施する。二台切替、既存 TCP/UDP 継続、実回線負荷は、この初回起動合格の後の別受入である。

## 空 state disk の確認

2026-11-01 に current main `3e2a73a` の通常 `make image` 出力から、空の0700 stage を指定して
`build-disk-image.py` で新規 `/dev/vda2` state partition を作成した。read-only mount では`lost+found`以外の
entry はなく、同じ root-only disk を隔離 mount namespace の QEMU で起動して Linux PID 1 の
`ROUTERD_READY`を37.7秒で確認した。これは state volume の空開始と boot path の確認であり、旧 state の
複写、Tailscale identity、lease/sticky import、対象機への切替は確認していない。disk と stage は確認後に削除する。
