package server

import (
	"encoding/json"
	"github.com/Autumn-27/artex/db"
	"testing"
)

func TestXiequSettingsDTOManagedIPsAlwaysArray(t *testing.T) {
	for _, tc := range []struct {
		name string
		ips  []string
		want string
	}{
		{"fresh", nil, `[]`}, {"empty", []string{}, `[]`},
		{"existing", []string{"203.0.113.8"}, `["203.0.113.8"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(xiequSettingsDTO(db.XiequConfig{}, db.XiequWhitelistState{ManagedIPs: tc.ips}))
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				State struct {
					ManagedIPs json.RawMessage `json:"managed_ips"`
				} `json:"state"`
			}
			if err := json.Unmarshal(payload, &response); err != nil {
				t.Fatal(err)
			}
			if string(response.State.ManagedIPs) != tc.want {
				t.Fatalf("managed_ips=%s，期望%s", response.State.ManagedIPs, tc.want)
			}
		})
	}
}
