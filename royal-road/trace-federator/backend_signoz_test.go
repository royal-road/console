package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A raw row in the shape SigNoz v0.144 returns from /api/v5/query_range
// (requestType raw, no selectFields); values are synthetic.
const signozRawRow = `{
  "attributes": {"db.system": "postgresql", "network.peer.port": 5432},
  "db_name": "", "db_operation": "", "duration_nano": 1500000, "events": [{"name": "exception", "timeUnixNano": 1794700000000500000}],
  "external_http_method": "", "external_http_url": "", "flags": 1, "has_error": true, "http_host": "", "http_method": "", "http_url": "",
  "is_remote": "no", "kind": 3, "kind_string": "Client",
  "links": [{"traceId": "11112222333344445555666677778888", "spanId": "1111222233334444"}],
  "name": "postgresql", "parent_span_id": "aaaabbbbccccdddd",
  "resource": {"service.name": "Web", "deployment.environment": "Production"},
  "response_status_code": "", "span_id": "0000000000000abc", "status_code": 2, "status_code_string": "Error", "status_message": "boom",
  "timestamp": "2026-11-15T00:00:00.0000005Z", "trace_id": "7a1b2c3d4e5f60718293a4b5c6d7e8f9", "trace_state": ""
}`

func TestSignozRowsToOTLP(t *testing.T) {
	var row map[string]any
	dec := json.NewDecoder(strings.NewReader(signozRawRow))
	dec.UseNumber()
	if err := dec.Decode(&row); err != nil {
		t.Fatal(err)
	}
	other := map[string]any{}
	for k, v := range row {
		other[k] = v
	}
	other["span_id"] = "0000000000000abd"
	other["resource"] = map[string]any{"service.name": "Other"}
	out := signozRowsToOTLP([]map[string]any{row, other})
	if len(out) != 2 {
		t.Fatalf("want 2 resource groups, got %d", len(out))
	}
	sp := out[0]["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)
	if sp["spanId"] != "0000000000000abc" || sp["parentSpanId"] != "aaaabbbbccccdddd" || sp["kind"] != 3 {
		t.Errorf("ids/kind: %v", sp)
	}
	if sp["startTimeUnixNano"] != "1794700800000000500" || sp["endTimeUnixNano"] != "1794700800001500500" {
		t.Errorf("times: %v %v", sp["startTimeUnixNano"], sp["endTimeUnixNano"])
	}
	if st := sp["status"].(map[string]any); st["code"] != 2 || st["message"] != "boom" {
		t.Errorf("status: %v", st)
	}
	b, _ := json.Marshal(out[0])
	for _, want := range []string{`"key":"network.peer.port","value":{"intValue":"5432"}`, `"key":"service.name","value":{"stringValue":"Web"}`, `"name":"exception"`, `"spanId":"1111222233334444"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
}
