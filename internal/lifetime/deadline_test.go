package lifetime

import (
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func TestOriginalProtocolBudgetSurvivesWallStepsAndExpiresInBoot(t *testing.T) {
	c := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0)}
	protocol := After(c, 30)
	cutoff := Deadline{UTC: c.WallTime.Add(time.Hour)}
	for _, step := range []time.Duration{24 * time.Hour, -24 * time.Hour, 0} {
		shifted := c
		shifted.WallTime = shifted.WallTime.Add(step)
		if protocol.Remaining(shifted) != 30*time.Second {
			t.Fatal("UTC changed original protocol budget")
		}
	}
	stepped := c
	stepped.WallTime = stepped.WallTime.Add(24 * time.Hour)
	if cutoff.Live(stepped) {
		t.Fatal("operator UTC cutoff ignored UTC")
	}
	expired := c
	expired.BoottimeNanoseconds += int64(30 * time.Second)
	if protocol.Live(expired) {
		t.Fatal("original boot budget did not expire")
	}
	expired = c
	expired.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if protocol.Live(expired) || After(c, ^uint32(0)).Live(expired) {
		t.Fatal("former-boot budget survived reboot")
	}
}
