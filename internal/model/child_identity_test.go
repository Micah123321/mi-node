package model

import (
	"encoding/json"
	"github.com/micah123321/mi-node/internal/panel"
	"reflect"
	"strconv"
	"testing"
)

func TestChildIdentityPanelRoundTrip(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("child identities require 64-bit ints")
	}
	var users []panel.User
	if err := json.Unmarshal([]byte(`[{"id":7,"uuid":"parent","speed_limit":8,"device_limit":2},{"id":1099511627777,"uuid":"child-one","speed_limit":8,"device_limit":2,"billing_user_id":7,"billing_node_id":11},{"id":1099511627778,"uuid":"child-two","speed_limit":8,"device_limit":2,"billing_user_id":7,"billing_node_id":12}]`), &users); err != nil {
		t.Fatal(err)
	}
	specs := UserSpecsFromPanel(users)
	if got := UserSpecsToPanel(specs); !reflect.DeepEqual(users, got) {
		t.Fatalf("round trip: %+v", got)
	}
	for _, u := range specs {
		if u.AccountID() != 7 {
			t.Fatalf("account ID: %+v", u)
		}
	}
	limits := AccountLimits(specs)
	if len(limits) != 1 || limits[7].SpeedLimit != 8 || limits[7].DeviceLimit != 2 {
		t.Fatalf("limits: %+v", limits)
	}
	specs[1].SpeedLimit, specs[1].DeviceLimit = 3, 1
	first := AccountLimits(specs)
	specs[0], specs[2] = specs[2], specs[0]
	if !reflect.DeepEqual(first, AccountLimits(specs)) || first[7].SpeedLimit != 3 || first[7].DeviceLimit != 1 {
		t.Fatal("alias limits are order dependent")
	}
}
