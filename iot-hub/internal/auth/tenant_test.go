package auth

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTenantViewsAreIsolated(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	a, b := root.Tenant("acme"), root.Tenant("globex")
	tokA, err := a.Add("pump", Device, []string{"*"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add("pump", Device, []string{"*"}, ""); err != nil {
		t.Fatalf("the same id in another tenant must be allowed: %v", err)
	}
	if id, ok := a.Authenticate(tokA); !ok || id.Tenant != "acme" {
		t.Fatalf("own tenant: %+v %v", id, ok)
	}
	if _, ok := b.Authenticate(tokA); ok {
		t.Fatal("acme's token accepted by globex's view")
	}
	if id, ok := root.Platform().Authenticate(tokA); !ok || id.Tenant != "acme" {
		t.Fatal("the platform view resolves every tenant")
	}
	if id, ok := b.Authenticate("super"); !ok || id.Role != Superadmin {
		t.Fatal("the superadmin is valid in every view")
	}
	if len(a.List()) != 1 || len(b.List()) != 1 || root.Platform().Count() != 2 {
		t.Fatalf("lists: %v %v", a.List(), b.List())
	}
	if err := b.Remove("pump"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate(tokA); !ok {
		t.Fatal("removing globex/pump must not touch acme/pump")
	}
	if _, err := root.Platform().Create(Spec{ID: "x", Role: Viewer}); err == nil {
		t.Fatal("identities must belong to a tenant")
	}
	ids, _ := root.RemoveTenant("acme")
	if len(ids) != 1 || root.Platform().Count() != 0 {
		t.Fatalf("remove tenant: %v", ids)
	}
}

func TestSASKeysRotationExpiry(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	root.c.now = func() time.Time { return now }
	acme := root.Tenant("acme")
	sec, err := acme.Create(Spec{ID: "pump-1", Role: Device, Sensors: []string{"pump-1"}, Auth: "keys"})
	if err != nil || sec.Token != "" || sec.PrimaryKey == "" || sec.SecondaryKey == "" {
		t.Fatalf("%+v %v", sec, err)
	}
	sas := func(key string, ttl time.Duration, resource string) string {
		tok, err := SignSAS(resource, key, now.Add(ttl))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	res := ResourceURI("acme", "pump-1")
	p := sas(sec.PrimaryKey, time.Hour, res)
	if !strings.HasPrefix(p, "SharedAccessSignature sr=acme%2Fdevices%2Fpump-1&sig=") {
		t.Fatalf("format %s", p)
	}
	if id, ok := acme.Authenticate(p); !ok || id.ID != "pump-1" || id.Tenant != "acme" {
		t.Fatal("primary-key SAS rejected")
	}
	if _, ok := acme.Authenticate(sas(sec.SecondaryKey, time.Hour, res)); !ok {
		t.Fatal("secondary-key SAS rejected")
	}
	other := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for name, tok := range map[string]string{
		"wrong key":       sas(other, time.Hour, res),
		"expired":         sas(sec.PrimaryKey, -time.Second, res),
		"too far ahead":   sas(sec.PrimaryKey, 400*24*time.Hour, res),
		"other device":    sas(sec.PrimaryKey, time.Hour, ResourceURI("acme", "pump-2")),
		"tampered expiry": strings.Replace(p, "&se=", "&se=9", 1),
		"garbage":         SASPrefix + "%%%",
	} {
		if _, ok := acme.Authenticate(tok); ok {
			t.Errorf("%s accepted", name)
		}
	}
	// The same key signing for another tenant's resource is not valid
	// there, and acme's SAS is not valid in globex's view.
	root.Tenant("globex").Create(Spec{ID: "pump-1", Role: Device, Sensors: []string{"*"}, Auth: "keys"})
	if _, ok := root.Tenant("globex").Authenticate(sas(sec.PrimaryKey, time.Hour, ResourceURI("globex", "pump-1"))); ok {
		t.Error("acme's key signs for globex")
	}
	if _, ok := root.Tenant("globex").Authenticate(p); ok {
		t.Error("acme's SAS accepted in globex's view")
	}

	// Rotation: the rotated key stops working, the other keeps working.
	rot, err := acme.Rotate("pump-1", "primary")
	if err != nil || rot.PrimaryKey == "" || rot.PrimaryKey == sec.PrimaryKey {
		t.Fatalf("rotate %+v %v", rot, err)
	}
	if _, ok := acme.Authenticate(p); ok {
		t.Error("SAS from the old primary still works")
	}
	if _, ok := acme.Authenticate(sas(sec.SecondaryKey, time.Hour, res)); !ok {
		t.Error("secondary stopped working during rotation")
	}
	if _, ok := acme.Authenticate(sas(rot.PrimaryKey, time.Hour, res)); !ok {
		t.Error("new primary rejected")
	}

	// Disabled devices and expired tokens are refused.
	disabled := true
	acme.Update("pump-1", Update{Disabled: &disabled})
	if _, ok := acme.Authenticate(sas(rot.PrimaryKey, time.Hour, res)); ok {
		t.Error("disabled device accepted")
	}
	tsec, _ := acme.Create(Spec{ID: "temp", Role: Viewer, Expires: now.Add(time.Hour).UnixMilli()})
	if _, ok := acme.Authenticate(tsec.Token); !ok {
		t.Error("token before expiry rejected")
	}
	now = now.Add(2 * time.Hour)
	if _, ok := acme.Authenticate(tsec.Token); ok {
		t.Error("expired token accepted")
	}
}

func TestKeysEncryptedAtRest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	// Written without a master key, then the operator adds one: existing
	// keys are encrypted on load and keep working.
	s := New(p, "super")
	sec, _ := s.Create(Spec{ID: "d", Role: Device, Sensors: []string{"*"}, Auth: "keys"})
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), sec.PrimaryKey) {
		t.Fatal("without a master key the key is stored as is")
	}
	s2 := New(p, "super")
	s2.SetMasterKey("correct horse battery staple, but longer and random")
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	if strings.Contains(string(raw), sec.PrimaryKey) || !strings.Contains(string(raw), `"v1:`) {
		t.Fatalf("keys not encrypted after adding a master key:\n%s", raw)
	}
	tok, _ := SignSAS(ResourceURI("default", "d"), sec.PrimaryKey, time.Now().Add(time.Hour))
	if _, ok := s2.Authenticate(tok); !ok {
		t.Fatal("encrypted key no longer verifies")
	}
	if k, err := s2.KeysOf("d"); err != nil || k.PrimaryKey != sec.PrimaryKey {
		t.Fatalf("keys shown to an admin: %v", err)
	}
	// Without the master key the hub can't use them, and says so.
	s3 := New(p, "super")
	s3.Load()
	if _, ok := s3.Authenticate(tok); ok {
		t.Fatal("verified without the master key")
	}
	if _, err := s3.KeysOf("d"); err == nil {
		t.Fatal("keys shown without the master key")
	}
}

func TestLegacyFileLoadsIntoDefaultTenant(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	tok := "iot_legacy"
	os.WriteFile(p, []byte(`[{"id":"old","role":"viewer","hash":"`+hashHex(tok)+`","created":1}]`), 0o600)
	s := New(p, "")
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if id, ok := s.Authenticate(tok); !ok || id.Tenant != DefaultTenant {
		t.Fatalf("%+v %v", id, ok)
	}
}
