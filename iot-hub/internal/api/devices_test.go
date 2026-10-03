package api_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
)

func TestDeviceKeysAndSAS(t *testing.T) {
	e := secure(t, false)
	code, m := do(t, nil, "POST", e.url+"/api/devices", adminTok, `{"id":"pump-1","role":"device","sensors":["pump-1"],"auth":"keys"}`)
	if code != 201 || m["token"] != nil || m["primaryKey"] == nil {
		t.Fatalf("create: %d %v", code, m)
	}
	cs := m["connectionString"].(string)
	if !strings.HasPrefix(cs, "HostName=127.0.0.1:") || !strings.Contains(cs, ";TenantId=default;DeviceId=pump-1;SharedAccessKey=") {
		t.Fatalf("connection string %s", cs)
	}
	key := m["primaryKey"].(string)
	sas, _ := auth.SignSAS(auth.ResourceURI("default", "pump-1"), key, time.Now().Add(time.Hour))
	ingest := func(authz string) int {
		code, _ := do(t, nil, "POST", e.url+"/api/sensors/pump-1/data", "", `{"rpm":1450}`, "Authorization", authz)
		return code
	}
	if c := ingest(sas); c != 202 {
		t.Fatalf("SAS ingest: %d", c)
	}
	// The SAS is scoped to the device's sensors like any identity.
	if code, _ := do(t, nil, "POST", e.url+"/api/sensors/pump-2/data", "", `{"rpm":1}`, "Authorization", sas); code != 403 {
		t.Fatalf("other sensor: %d", code)
	}
	// Server-issued SAS for devices that can't sign.
	code, m = do(t, nil, "POST", e.url+"/api/devices/pump-1/sas", adminTok, `{"ttl":"2h"}`)
	if code != 200 || ingest(m["sas"].(string)) != 202 {
		t.Fatalf("issued sas: %d %v", code, m)
	}
	// Rotating the primary ends it; the secondary keeps working.
	code, m = do(t, nil, "GET", e.url+"/api/devices/pump-1/keys", adminTok, "")
	second := m["secondaryKey"].(string)
	do(t, nil, "POST", e.url+"/api/devices/pump-1/rotate", adminTok, `{"which":"primary"}`)
	sas2, _ := auth.SignSAS(auth.ResourceURI("default", "pump-1"), second, time.Now().Add(time.Hour))
	if ingest(sas) != 401 || ingest(sas2) != 202 {
		t.Fatal("rotation: old primary must fail, secondary must work")
	}
	// Disable.
	if code, _ = do(t, nil, "PATCH", e.url+"/api/devices/pump-1", adminTok, `{"disabled":true}`); code != 204 || ingest(sas2) != 401 {
		t.Fatalf("disable: %d", code)
	}
	code, list := do(t, nil, "GET", e.url+"/api/devices", adminTok, "")
	_ = list
	if code != 200 {
		t.Fatal(code)
	}
	// Expiring tokens.
	exp := time.Now().Add(time.Hour).UnixMilli()
	code, m = do(t, nil, "POST", e.url+"/api/devices", adminTok, fmt.Sprintf(`{"id":"temp","role":"viewer","expires":%d}`, exp))
	if code != 201 || m["token"] == nil {
		t.Fatalf("expiring token: %d %v", code, m)
	}
	if code, _ = do(t, nil, "POST", e.url+"/api/devices", adminTok, `{"id":"past","role":"viewer","expires":1}`); code != 400 {
		t.Errorf("expiry in the past: %d", code)
	}
}
