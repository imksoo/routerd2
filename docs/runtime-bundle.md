# 通常ビルドから署名runtimeを作る

`make runtime-bundle`はLinuxの通常ビルドと既存のoffline publisherを使い、
3実行物、boot catalogが指定するengineと対応source、nDPIの対応source、
プロジェクトと依存関係のlicense資料を一つの署名tarへまとめる。
署名鍵はoperatorが指定する。bootから取り込むのはrole/engine catalogであり、
siteの起動設定や回線設定を実行時設定として同梱しない。

```sh
make runtime-bundle DOCKER='sudo -n docker' \
  BUNDLE_SIGNING_KEY=/secure/routerd-signing.pem \
  BUNDLE_SEQUENCE=42
```

鍵は既存のowner専用PKCS#8 Ed25519 PEMを指定する。sequenceは同じpublisherで
単調増加させる。既存の出力ファイルは置き換えない。
通常の出力は`dist/routerd-runtime-<release-id>-<arch>.tar`。
`BUNDLE_OUTPUT`で新しい出力先、`BUNDLE_BOOT`でboot catalogを指定できる。
既定は`images/boot.json`で、現在は25役割・5 engineを宣言している。
この入口はbuild host上で動くnative Linux build向け。異なるarchitectureの実行資格は別に確認する。

publisherへ渡す前に、選択したビルド成果物をowner専用の一時領域へ固定する。
共有worktreeの権限変更は行わず、一時領域は成功・失敗のどちらでも撤去する。
role/engine catalogと署名・artifact検証は既存のpublisher/receiverへ委ねる。
bootの実行物pathはartifact名へ結び付け、site固有のboot設定自体は格納しない。

公開trust mapは既存publisherで別に出力する。秘密鍵は受信側へ配らない。

```sh
dist/bin/routerd-bundle trust --private-key-file /secure/routerd-signing.pem > release-keys.json
```

受信側の例。storeと公開trust mapを明示して署名を検証し、変更しないruntime directoryへ展開する。

```sh
routerd release stage --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64 \
  --file routerd-runtime-<release-id>-amd64.tar
routerd release verify --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64 \
  --digest <stageが返したdigest>
```

初回登録は空storeへの`release initialize`、候補の準備・取消は現在のgenerationを取得した
`release prepare` / `release cancel`を使う。これらは保存する選択を操作し、起動や通信確認は行わない。
実運用の更新にはstartup-configとの組合せ、PID 1の起動・確認と復旧が必要。
[boot/runtime設計](adr/0001-boot-runtime-startup-config.md)と[試験計画](test-plan.md)を参照する。

2026-11-03、通常build `842c47e32300`で全入口を実行した。
Linux amd64、25役割・5 engine・15 artifacts、527,315,456 byte。
実receiverのstage/verify/initialize/status、続く別digestのprepare/cancelが終了0。
取消後も元のactiveとpublisher watermarkを維持し、署名済みlicense archive内の
303ファイルにプロジェクトLICENSEと依存inventoryが含まれることを確認した。
これは配布物と保存選択の受入であり、このbundleによる実機boot・全機能更新の結果ではない。

nDPIのsource収録もproduction sourceと明示したビルド入力に限定する。
worktree直下のprivate設定probeと`images/boot.json`がsource archiveへ入らず、
production sourceとLICENSEが含まれることを確認した。署名は独立OpenSSLでも検証した。

2026-11-03、基準main `b3a0053` + 本変更の通常build `b3a0053f24c6`で、全25役割・5 engineの署名bundleを実receiverでstage/initializeし、Linux namespaceの実PID 1から起動した。独立RP-PPPoE/ISC DHCPv6/Linux LAN端末とのPAP・CHAPが両方終了0。DNS UDP/TCP、両IP TCP/UDP、自然Renew、server停止によるPD期限切れ/再取得、worker交換/DUID保持/古い通知拒否、confirmed rollback、PD/RA削除復元、PPP MTU変更、手動/control再起動、正常poweroffが通過。captureでMTU変更・手動/control再起動のRA zero-lifetime通知がPADTより前であり、上流Releaseなしと確認した。 これは実kernel/PID 1と独立peerでの配布boot・PPP/PD復旧の受入。ISPのPPPoE、ONU/NIC実機、全homert profileの署名更新/rollback、全34 packageの完成は残る。homert上流候補へPPPoEや追加NICは導入していない。

実通信での更新・復帰: 2026-11-03、通常署名bundleを実control APIでupload/stage/prepareし、実PID 1のrebootと再起動で `b3a0053f24c6 → 30054e56fdf6 → b3a0053f24c6` の更新・明示的rollbackが終了0。独立RP-PPPoE/ISC DHCPv6/Linux LANを使用し、全workerのrelease ID、実health watcherの自動confirm、現在の設定/DUID保持、DNS UDP/TCPと両IP TCP/UDP、high watermark=2の保持を確認した。候補でDNSレコードを追加してから旧runtimeへ戻し、その追加設定の両transportでの実回答も維持した。startup採用時は設計どおりrevisionが進む。正常poweroffとcaptureの上流PD Releaseなしも通過。 製品Goは両buildで同一で、異なる通常release IDの実行物を選択・execする経路の確認。PID namespaceを同じkernel上で再起動した試験であり、kernel/bootloader再起動、停電、失敗候補の自動fallback、異なるABI/実装間の更新、全homert profile/ONU/NIC資格は残る。全構成のappliance NTPには独立kernel/時計の実行機が必要で、共有host時計は変更していない。PVE/QEMUは実行していない。 再実行条件は[試験計画](test-plan.md)を参照する。
