# 可変artifactの署名runtime・role catalog

2026-10-25 UTC。product checkpoint `f7da1c2b0016727c5681b83485e672788af8f8c7`。
disk probe fixture `e6720b9f8e7225bdbc90e18b4ccd209f477f87a0`、
kernel test image fixture `c88a89775ef223745b51a9b0b63c7f77ab603f5d`。
CORE-06／DIST-01の部分実装。全計画、全module分割、対象実機の受入は完了していない。

## 実装した境界

旧manifest v1の二ELF固定を、manifest v2へ置換した。互換readerはない。
署名domainも `routerd2 signed extracted runtime manifest v2\x00` に変え、旧domainの署名を受理しない。
manifestはboot/worker ABI、entrypoint、role→artifact、artifact kind・Go command identity・size/SHAを含む。
全payloadをdigestで特定するversioned directoryへ展開し、selectionとは別に公開する。

最大64 artifact／64 role、manifest64KiB、各artifact1GiB、総payload4GiB。
receiverの既定は各artifact512MiB。初期layoutはflatな相対filename、static native Go ELFとregular dataのみ。
directory・link・特殊file・PAX/GNU拡張、宣言外file、重複、path逸脱、不正ABI/target/size/digestを拒否する。
nativeは0500、dataは0400。dataの改竄もruntime全体の再検証失敗にする。
外部engine、dynamic loader/library/plugin、nested layoutの受入は残作業であり、現形式では拒否する。

publisherは `--layout` で明示したsource file集合を開き、検証・署名・出力時の再hashを行う。
source pathは署名manifestへ入らない。`--routerd`／`--modules` の旧暗黙pair入力は拒否する。
[layout例](../../examples/runtime-layout.json) と [実行手順](../releases.md) を参照。

`Image`は全artifactとrole catalogを保持する。bootは署名済みentrypointのdescriptorでPID 1を引き継ぎ、
supervisorはroleごとのfileを一度だけ開き、各再起動も同じinodeに固定する。
dynamic worker APIは引き続きname／role／config digestだけを受け付け、pathやargvを受け取らない。
bootの `workers` mapはserialized inputの宣言だけを使い、factoryのdefault roleを暗黙に追加しない。
旧 `module_path` は拒否する。native標準の固定argument contractはworker ABI 1であり、任意argvはmanifestにない。

factory commandは `routerd`、`routerd-modules`、独立した `routerd-logging` の3件になった。
logging binaryは約6.9MiBで、logging以外のroleを拒否する。control／management／protocol engineへの依存はない。
APK、OCI、image assembler、MakefileとQEMU buildersへ追加した。binaryの分割だけで権限分離の完了とは扱わない。
OCI build133.02秒・network none/read-only containerの起動/control/正常終了1.19秒PASS。
Alpine APKはabuildのunit testと全3 ELFのstatic検査を含め266.83秒PASS。APKから実3 ELFを取り出し、
署名runtimeで使う同じoffline verifierでtarget/Go command/release identityを検証した（1.03秒PASS）。
抽出したAPK実バイナリーのSSH/config/confirmed rollback/再起動復旧もRace harness9.35秒PASS。
APKのSHAとnativeビルドのSHAは別に記録し、同じbyte列と主張しない。
private exporter出力のroot所有0700による最初の非root列挙失敗は保存し、このtaskの新しい出力だけを所有権変更して再収集した。

## ローカル検証

| 検証 | 結果 |
| --- | --- |
| 初期catalogの全release package Race | 73.578秒PASS |
| roleごとの異なるinode・path差替え後の再起動 | Race3.277秒PASS |
| 最終productの全体check／Race／vet／Python24／6 ELF build | 27.00秒PASS、clean再確認11.04秒PASS |
| disk probe修正後の全体check/build | 12.82秒PASS、clean11.37秒PASS |
| module closure修正後の全体check／Python27／6 ELF build | 13.03秒PASS、clean11.43秒PASS |
| 実amd64/arm64の3 commandを署名・stage・verify・選択 | 27.01秒PASS（arm64は実行しない） |
| offline publisherで実3 commandを作成・stage | 12.76秒PASS |
| native実DHCP期限・再送・worker/control復旧、Race harness | 10.25秒PASS |
| native実SSH編集・confirmed rollback・再起動復旧、Race harness | 9.30秒PASS |
| native二WAN Health→PBR・TCP/UDP/PCAP・rollback、Race harness | 31.12秒PASS |

最後のfixture変更を含め、6 ELFのSHAはchecked productと同一。
nativeはroot所有0700のprivate stageを使い、ネットワーク試験はfresh user/network namespaceで実行した。
ホストnetwork／clockやhome/repositoryのmodeを変更していない。

## 失敗の保存と局所再現

最初のQEMU release fixtureは、同じ大きなmulticall ELFを3つのartifactとして複製していた。
trial展開中の容量不足で43.93秒FAIL（host56.53秒）。そのbytesと失敗記録は保存した。
private mount namespaceの256MiB tmpfsで2版のstageを実行し、複製構成はENOSPC、
独立loggingを含む3 command構成は同じ上限でPASSした。初回33.38秒、clean再現33.09秒。

次のdisk試験は旧filenameを探すfixtureでhost101.86秒FAILした。
署名catalogのentrypoint／roleからdescriptorを得て、実processのinodeを照合する局所試験を追加した。
missing API RED後にRace1.031秒PASS。旧filenameや互換linkを追加していない。

通常daemonのQEMU試験はさらに`text file busy`でFAILした。
ローカルで開いたwriterがあるとexecがETXTBSYになり、閉じると同じdescriptorで起動することを確認した。
guestのread-only FD診断ではuserspace writerが見つからず、path／child FD／parent FDのいずれも失敗した。
同一bytesを別inodeへコピーした試験ではSIGSEGVとなった。これらの観測用runも保存し、合格扱いにしていない。

early serialを確認すると、test開始より前にkernelが `Initramfs unpacking failed: write error` を報告していた。
全kernel moduleを含めたpayload198.8MiBが、512MiB guestのrootfsへ完全に展開されず、
不完全なnative ELFを使っていた。これを製品のFD実行失敗と混同していた。
専用imageは固定17 rootとその依存35 moduleへ絞り、約114.4MiBの不要moduleを除いた。
共有JSONをGo initとbuilderの両方が読み、要求と梱包対象のずれを防ぐ。

192MiBの実tmpfs上で旧imageのENOSPCを再現し、同じ上限でpayload84.4MiBの新image全体が展開できた。
kernelのunpack errorがあれば、後続のPASS markerがあってもgateをFAILにする。
RAM・CPU・rootfs mount設定・scratch・既存fixture期限は変更していない。

## QEMU受入の現在値

- release boot：83.25秒PASS。実PID 1 exec、署名4 artifact／独立native3／data1、各core inode、health confirmation。
- 永続ext4 disk：2 boot合計148.50秒PASS。異なるboot ID、未確認trialからbaselineへfallback、
  SSH編集revision2・host key・state sentinel・durable event ID/cursorを保持し、新kernelへ再適用。
- 通常daemon3 gate：SSH26.85秒、二WAN Health→PBR78.52秒、実UTC補正29.22秒PASS。
  3 gateは同じinitializer/test binary/kernel/3 commandのSHAを使い、必要35 moduleとprivate scratchを保持する。

release/storageは元の1024MiB／2 CPU、通常daemonは512MiB／2 CPU／private scratch64MiBで外部NICなし。
異なるfixture checkpointの個別gateであり、新しい全suite一括受入ではない。

## 公開前の検査

変更したcode／文書／公開証跡をindexから検査した。scanは既存の無効PEM fixture 1件を検出したが、
元mainと同じ文字列でbase64検証が失敗することを確認した。実secretではなく、fixture変更やscan抑制はしていない。
公開証跡の試験用SSH private-key hashは省略し、公開fingerprintとraw検証のSHAを保持した。
[最終fixture判定](2026-10-25-runtime-catalog/final-known-invalid-fixture-review.json) を参照。

## 残作業

明示startup-config store/revisionの選択transaction、小さな独立bootstrap、config/state/kernel ABIの完全なpreflight、
external engineと依存loader/library/plugin、全source/SBOM/noticesの配布受入、worker権限分離を続ける。
factoryの3 commandは最初の分割であり、全moduleの独立binary化は完了していない。
arm64実行、対象実機/NIC、長時間・性能、whole-OS installer/A/Bと全残計画は未完了。
work package全体やparityを完了扱いにせず、継続ゴールをactiveに保つ。

[provenance](2026-10-25-runtime-catalog/provenance.json) にsource、元log SHA、fixtureごとの区別、環境とprivate PCAPのSHAを記録する。
公開artifactのSHAは `2026-10-25-runtime-catalog/SHA256SUMS` にある。ELF、initramfs、private disk、PCAP本体はcommitしない。
