package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCanMatrix(t *testing.T) {
	dev := &Identity{ID: "d", Role: Device, Sensors: []string{"env-*"}}
	svc := &Identity{ID: "s", Role: Service, Sensors: []string{"*-ml"}}
	view := &Identity{ID: "v", Role: Viewer}
	adm := &Identity{ID: "a", Role: Admin}
	cases := []struct {
		id     *Identity
		a      Action
		sensor string
		want   bool
	}{
		{dev, Ingest, "env-1", true}, {dev, Ingest, "power-1", false}, {dev, Read, "", false},
		{dev, Subscribe, "", false}, {dev, Define, "env-1", false}, {dev, Manage, "", false},
		{svc, Read, "", true}, {svc, Subscribe, "", true}, {svc, Ingest, "access-1-ml", true},
		{svc, Ingest, "access-1", false}, {svc, Define, "x-ml", true}, {svc, Manage, "", false},
		{view, Read, "", true}, {view, Subscribe, "", true}, {view, Ingest, "env-1", false}, {view, Manage, "", false},
		{adm, Manage, "", true}, {adm, Ingest, "anything", true}, {adm, Platform, "", false},
		{&Identity{Role: Superadmin}, Platform, "", true},
		{nil, Read, "", false},
	}
	for _, c := range cases {
		if got := c.id.Can(c.a, c.sensor); got != c.want {
			id := "nil"
			if c.id != nil {
				id = c.id.ID
			}
			t.Errorf("%s can %d on %q = %v, want %v", id, c.a, c.sensor, got, c.want)
		}
	}
}

func TestStoreLifecycle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "devices.json")
	s := New(p, "")
	if !s.Enabled() == false {
		t.Fatal("no credentials: auth must be off")
	}
	if id, ok := s.Authenticate("whatever"); !ok || id != Anonymous {
		t.Fatal("auth off: everyone is anonymous admin")
	}
	tok, err := s.Add("env-node", Device, []string{"env-*"}, "roof")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("env-node", Device, []string{"x"}, ""); err != ErrExists {
		t.Fatalf("duplicate: %v", err)
	}
	for _, bad := range []struct {
		id   string
		role Role
		sens []string
	}{{"admin", Viewer, nil}, {"has space", Viewer, nil}, {"x", "root", nil}, {"y", Device, nil}, {"z", Device, []string{"["}}} {
		if _, err := s.Add(bad.id, bad.role, bad.sens, ""); err == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}
	if !s.Enabled() {
		t.Fatal("auth must turn on with the first credential")
	}
	id, ok := s.Authenticate(tok)
	if !ok || id.ID != "env-node" || id.Role != Device {
		t.Fatalf("authenticate: %+v %v", id, ok)
	}
	if _, ok := s.Authenticate(tok + "x"); ok {
		t.Fatal("wrong token accepted")
	}
	b, _ := os.ReadFile(p)
	// Windows has no POSIX mode bits (access is by ACL), and Go reports 0666
	// there whatever the file was created with.
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	if contains(b, tok) {
		t.Fatal("token stored in plaintext")
	}
	s2 := New(p, "")
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if id, ok := s2.Authenticate(tok); !ok || id.ID != "env-node" {
		t.Fatal("credentials must survive reload")
	}
	if err := s2.Remove("env-node"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Authenticate(tok); ok {
		t.Fatal("revoked token accepted")
	}
	if !s2.Enabled() {
		t.Fatal("revoking the last credential must not turn authentication off")
	}
	s3 := New(p, "")
	_ = s3.Load()
	if _, ok := s3.Authenticate(""); ok || !s3.Enabled() {
		t.Fatal("an empty credentials file still means auth is on")
	}
}

func TestAdminToken(t *testing.T) {
	s := New("", "boot")
	if id, ok := s.Authenticate("boot"); !ok || id.Role != Superadmin || !id.Can(Platform, "") || !id.InTenant("any") {
		t.Fatal("the -token identity is the superadmin")
	}
	if _, ok := s.Authenticate(""); ok {
		t.Fatal("empty token accepted")
	}
}

func contains(b []byte, s string) bool {
	return len(s) > 0 && string(b) != "" && (func() bool {
		for i := 0; i+len(s) <= len(b); i++ {
			if string(b[i:i+len(s)]) == s {
				return true
			}
		}
		return false
	})()
}
