package panel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestWSDevicesAcceptsPHPDeviceLists(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    map[int][]string
	}{
		{"normal", `{"node_id":526,"users":{"3401":["192.0.2.1","2001:db8::1"],"3421":["192.0.2.2"]}}`,
			map[int][]string{3401: {"192.0.2.1", "2001:db8::1"}, 3421: {"192.0.2.2"}}},
		// PHP array_unique preserves sparse indexes and json_encode emits an object.
		{"sparse PHP indexes", `{"node_id":"526","users":{"3401":{"2":"2001:db8::1","0":"192.0.2.1","10":"192.0.2.3"},"3421":["192.0.2.2"]}}`,
			map[int][]string{3401: {"192.0.2.1", "2001:db8::1", "192.0.2.3"}, 3421: {"192.0.2.2"}}},
		{"empty PHP users", `{"node_id":526,"users":[]}`, map[int][]string{}},
		{"empty JSON object", `{"node_id":526,"users":{}}`, map[int][]string{}},
		{"empty user lists", `{"node_id":526,"users":{"3401":[],"3421":{}}}`,
			map[int][]string{3401: {}, 3421: {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received []WSEvent
			client := &WSClient{onEvent: func(event WSEvent) { received = append(received, event) }}
			client.handleDataEvent(wsMessage{Event: WSEventSyncDevices, Data: json.RawMessage(tc.payload)})
			if len(received) != 1 {
				t.Fatalf("device snapshot was dropped: %s", tc.payload)
			}
			if received[0].Type != WSEventSyncDevices || received[0].NodeID != 526 {
				t.Fatalf("lost device routing: %+v", received[0])
			}
			if !reflect.DeepEqual(received[0].DeviceUsers, tc.want) {
				t.Fatalf("devices = %#v, want %#v", received[0].DeviceUsers, tc.want)
			}
		})
	}
}

func TestWSDevicesRejectsUnknownDeviceObjects(t *testing.T) {
	for _, payload := range []string{
		`{"users":{"3401":{"ip":"192.0.2.1"}}}`,
		`{"users":{"3401":{"-1":"192.0.2.1"}}}`,
		`{"users":{"3401":{"01":"192.0.2.1"}}}`,
		`{"users":{"3401":{"0":{"ip":"192.0.2.1"}}}}`,
	} {
		client := &WSClient{onEvent: func(WSEvent) { t.Errorf("malformed snapshot was delivered: %s", payload) }}
		client.handleDataEvent(wsMessage{Event: WSEventSyncDevices, Data: json.RawMessage(payload)})
	}
}
