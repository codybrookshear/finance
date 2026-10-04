package web

import (
	"fmt"
	"testing"
	"time"
)

func TestNextSync(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	for in, want := range map[string]string{
		"00:10": "01:40", "01:40": "04:40", "01:41": "04:40", "13:00": "13:40",
		"22:39": "22:40", "22:41": "01:40 +1",
	} {
		h, m := 0, 0
		fmt.Sscanf(in, "%d:%d", &h, &m)
		now := time.Date(2026, 10, 3, h, m, 0, 0, la)
		got := nextSync(now)
		s := got.Format("15:04")
		if got.Day() != now.Day() {
			s += " +1"
		}
		if s != want {
			t.Errorf("nextSync(%s) = %s, want %s", in, s, want)
		}
	}
}
