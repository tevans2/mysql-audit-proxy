package audit

import (
	"encoding/json"
	"reflect"
	"testing"
)

/*
 * TestEventJSONContract pins Event's wire format to the example in
 * docs/design/data-model.md §Ingest. It is the check that catches the proxy
 * and the API drifting apart, so the expected JSON below is copied from the
 * doc rather than derived from the struct.
 */
func TestEventJSONContract(t *testing.T) {
	e := Event{
		ConnID:       "c46e3089-a29d-4c3e-8c3b-1b4e5d6a7f8e",
		Kind:         KindConnect,
		At:           "2026-08-12T19:00:00Z",
		User:         "app_user",
		ClientAddr:   "192.168.1.10:54321",
		ProxyPort:    3307,
		Upstream:     "db-primary",
		UpstreamAddr: "10.0.0.5:3306",
		AuthResult:   "ok",
	}

	const want = `{
		"conn_id":"c46e3089-a29d-4c3e-8c3b-1b4e5d6a7f8e",
		"kind":"connect",
		"at":"2026-08-12T19:00:00Z",
		"user":"app_user",
		"client_addr":"192.168.1.10:54321",
		"proxy_port":3307,
		"upstream":"db-primary",
		"upstream_addr":"10.0.0.5:3306",
		"auth_result":"ok"
	}`

	got, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Compared as maps, not strings: field order is an implementation detail,
	// but an unexpected or missing key is not.
	var gotMap, wantMap map[string]any
	if err := json.Unmarshal(got, &gotMap); err != nil {
		t.Fatalf("unmarshal produced JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantMap); err != nil {
		t.Fatalf("unmarshal expected JSON: %v", err)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Errorf("Event JSON does not match the contract\n got: %v\nwant: %v", gotMap, wantMap)
	}
}
