# 永続観測履歴のSSH CLI

2026-10-24 UTC。コードcheckpoint: `39f52e0`。

`show observations [after CURSOR] [limit 1..16]` を実装した。
一度に1ページだけ読み、返されたnext_cursorで継続する。既定はafter 0 / limit 16。
運用・candidateの両モードで使え、candidateを変更しない。
補完とhelpを追加し、重複・未知optionや範囲外のcursor/limitはAPI呼出し前に拒否する。
storage read失敗時は部分履歴を出さない。

CLI/managementの全Race試験、引数・失敗・補完の回帰試験がPASS。
clean checkpointで `make check build-linux`（unit/Python/Race/vet/static amd64・arm64）もPASS。
実SSHの設定commit/confirm/rollbackとcontrol SIGKILLの後、同じSSH接続からコマンドを実行し、
永続APIの全recordと一致することをQEMUで確認した（36.76秒）。

[check/build log](2026-10-24-observations-cli/make-check-build.log)、
[CLI Race log](2026-10-24-observations-cli/cli-race.log)、
[補完回帰log](2026-10-24-observations-cli/completion-race.log)、
[QEMU log](2026-10-24-observations-cli/qemu.log)、
[ELF/kernel hash付きmanifest](2026-10-24-observations-cli/qemu-manifest.json)、
[provenance](2026-10-24-observations-cli/provenance.json)、
[SHA256SUMS](2026-10-24-observations-cli/SHA256SUMS) を保存する。

履歴は同じjournal内のcursorであり、kernel操作の許可や有限protocol factではない。
EventRule/DerivedEvent、consumer作用、protocol event、federation、retention等は未完了。
CORE-04/MGMT-01や全実装計画、実機利用可能性の全体合格とはしない。
