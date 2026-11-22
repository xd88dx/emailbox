package configs

import "testing"

func TestValidateUI(t *testing.T) {
	cases := []struct {
		minutes int
		wantErr bool
	}{
		{minutes: 0},
		{minutes: 5},
		{minutes: 60},
		{minutes: 1440},
		{minutes: 4, wantErr: true},
		{minutes: -1, wantErr: true},
		{minutes: 1441, wantErr: true},
	}
	for _, c := range cases {
		err := validateUI(&Config{UI: UIConfig{AutoRefreshMinutes: c.minutes}})
		if (err != nil) != c.wantErr {
			t.Errorf("UI_AUTO_REFRESH_MINUTES=%d: err=%v, wantErr=%v", c.minutes, err, c.wantErr)
		}
	}
}