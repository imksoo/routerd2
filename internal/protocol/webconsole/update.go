// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/release"
)

//go:embed update.html
var updateHTML string

type updatePage struct {
	Running    buildinfo.Info
	Generation uint64
	ChangedAt  time.Time
	Operation  string
	State      string
	Releases   []updateSelection
	KnownGood  *release.Ref
	Trial      *updateTrial
	Failure    *updateFailure
}

type updateSelection struct {
	Kind    string
	Label   string
	Release release.Ref
	Startup *configref.Selection
}

// The attempt token is authority to confirm or fail a boot, not display data.
type updateTrial struct {
	Number      int
	MaxAttempts int
	StartedAt   time.Time
}

type updateFailure struct {
	Release release.Ref
	Reason  string
	At      time.Time
}

func loadUpdate(ctx context.Context, client *localapi.Client) (*updatePage, error) {
	var state release.State
	if err := client.Do(ctx, http.MethodGet, "/v1/releases/status", nil, &state); err != nil {
		return nil, fmt.Errorf("署名付き更新の状態を取得できません: %w", err)
	}
	p := &updatePage{Generation: state.Generation, ChangedAt: state.ChangedAt, State: "起動する版が未選択"}
	if err := client.Do(ctx, http.MethodGet, "/v1/version", nil, &p.Running); err != nil {
		return nil, fmt.Errorf("制御担当の実行版を取得できません: %w", err)
	}
	switch state.Operation {
	case "empty":
		p.Operation = "未初期化"
	case "initialize":
		p.Operation = "基準版の登録"
	case "prepare":
		p.Operation = "次回起動の予約"
	case "prepare-rollback":
		p.Operation = "前版への復帰を予約"
	case "begin-boot":
		p.Operation = "試行起動"
	case "confirm":
		p.Operation = "正常確定"
	case "cancel":
		p.Operation = "予約の取消"
	case "rollback":
		p.Operation = "前版への選択変更"
	case "boot-failed":
		p.Operation = "試行失敗（復帰先なし）"
	case "boot-rollback":
		p.Operation = "試行失敗による復帰先の選択"
	default:
		p.Operation = state.Operation
	}
	if state.Active != nil {
		p.State = "確定版を選択中"
		p.KnownGood = state.Active
		p.Releases = append(p.Releases, updateSelection{Kind: "active", Label: "起動に選択された版", Release: *state.Active, Startup: state.Startup})
	}
	if state.Pending != nil {
		p.State = "次回起動待ち"
		p.Releases = append(p.Releases, updateSelection{Kind: "pending", Label: "予約された版", Release: state.Pending.Release, Startup: state.Pending.Startup})
	}
	if state.Previous != nil {
		p.Releases = append(p.Releases, updateSelection{Kind: "previous", Label: "前版（復帰候補）", Release: *state.Previous, Startup: state.PreviousStartup})
	}
	if state.Attempt != nil {
		p.State = "試行中（正常確定待ち）"
		p.KnownGood = state.Previous
		p.Trial = &updateTrial{Number: state.Attempt.Number, MaxAttempts: state.Pending.MaxAttempts, StartedAt: state.Attempt.StartedAt}
	}
	if state.LastFailure != nil {
		p.Failure = &updateFailure{Release: state.LastFailure.Release, Reason: state.LastFailure.Reason, At: state.LastFailure.At}
	}
	return p, nil
}
