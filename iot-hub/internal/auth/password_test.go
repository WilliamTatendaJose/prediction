package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func person(t *testing.T, s *Store, id, email, pw string, role Role) {
	t.Helper()
	if _, err := s.Create(Spec{ID: id, Role: role, Auth: "password", Email: email, Password: pw}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func TestPasswordSignIn(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	acme := root.Tenant("acme")
	person(t, acme, "ana", "Ana@Plant.CO", "correct horse battery", Admin)

	// The address is normalised: case and surrounding space don't matter.
	for _, email := range []string{"ana@plant.co", "ANA@PLANT.CO", "  Ana@Plant.co  "} {
		if id, ok := acme.AuthenticatePassword(email, "correct horse battery"); !ok || id.ID != "ana" || id.Role != Admin {
			t.Errorf("sign in with %q: %+v %v", email, id, ok)
		}
	}
	for _, tc := range []struct{ email, pw string }{
		{"ana@plant.co", "wrong"},
		{"ana@plant.co", ""},
		{"nobody@plant.co", "correct horse battery"},
		{"not an address", "correct horse battery"},
	} {
		if _, ok := acme.AuthenticatePassword(tc.email, tc.pw); ok {
			t.Errorf("accepted %q / %q", tc.email, tc.pw)
		}
	}
	// A password sign-in issues no token, so there is nothing to leak.
	for _, d := range acme.List() {
		if d.ID == "ana" && (d.Token || !d.Password || d.Email != "ana@plant.co") {
			t.Errorf("listing %+v", d)
		}
	}
}

func TestPasswordRules(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	acme := root.Tenant("acme")
	for _, tc := range []struct {
		name, email, pw string
		role            Role
	}{
		{"short password", "a@b.co", "short", Viewer},
		{"no email", "", "a long enough password", Viewer},
		{"bad email", "not-an-address", "a long enough password", Viewer},
		{"device with a password", "d@b.co", "a long enough password", Device},
		{"service with a password", "s@b.co", "a long enough password", Service},
	} {
		sp := Spec{ID: "x-" + strings.ReplaceAll(tc.name, " ", "-"), Role: tc.role, Email: tc.email, Password: tc.pw}
		if tc.role == Device || tc.role == Service {
			sp.Sensors = []string{"*"}
		}
		if _, err := acme.Create(sp); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
	if len(strings.Repeat("x", MaxPassword+1)) <= MaxPassword {
		t.Fatal("test setup")
	}
	if _, err := acme.Create(Spec{ID: "huge", Role: Viewer, Email: "h@b.co", Password: strings.Repeat("x", MaxPassword+1)}); err == nil {
		t.Error("a password over the limit was accepted")
	}
}

// An address identifies an account before any tenant is known, so it has to
// be unique platform-wide.
func TestEmailIsUniqueAcrossTenants(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	person(t, root.Tenant("acme"), "ana", "ana@plant.co", "correct horse battery", Admin)
	if _, err := root.Tenant("globex").Create(Spec{ID: "ana2", Role: Admin, Auth: "password",
		Email: "ANA@plant.co", Password: "another good password"}); err == nil {
		t.Fatal("the same address was accepted in a second tenant")
	}
	// Still free for the account that holds it.
	if err := root.Tenant("acme").SetPassword("ana", "ana@plant.co", "a replacement password"); err != nil {
		t.Fatalf("keeping your own address: %v", err)
	}
}

func TestSessions(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	acme := root.Tenant("acme")
	person(t, acme, "ana", "ana@plant.co", "correct horse battery", Operator)
	id, ok := acme.AuthenticatePassword("ana@plant.co", "correct horse battery")
	if !ok {
		t.Fatal("sign in")
	}
	ses, err := acme.StartSession(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !IsSession(ses) || strings.Contains(ses, "correct") {
		t.Fatalf("session token %q", ses)
	}
	if got, ok := acme.Authenticate(ses); !ok || got.ID != "ana" || got.Role != Operator {
		t.Fatalf("session authenticates: %+v %v", got, ok)
	}
	// Another tenant's view must not accept it.
	if _, ok := root.Tenant("globex").Authenticate(ses); ok {
		t.Error("another tenant accepted the session")
	}
	// Signing out ends it server-side.
	acme.EndSession(ses)
	if _, ok := acme.Authenticate(ses); ok {
		t.Error("session still works after sign-out")
	}
}

func TestSessionFollowsTheAccount(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	acme := root.Tenant("acme")
	start := func(user string) string {
		t.Helper()
		id, ok := acme.AuthenticatePassword(user+"@plant.co", "correct horse battery")
		if !ok {
			t.Fatalf("sign in as %s", user)
		}
		ses, err := acme.StartSession(id, 0)
		if err != nil {
			t.Fatal(err)
		}
		return ses
	}
	for _, u := range []string{"dis", "del", "pw", "exp"} {
		person(t, acme, u, u+"@plant.co", "correct horse battery", Operator)
	}
	disabled, deleted, changed, expired := start("dis"), start("del"), start("pw"), start("exp")

	yes := true
	if err := acme.Update("dis", Update{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := acme.Remove("del"); err != nil {
		t.Fatal(err)
	}
	if err := acme.SetPassword("pw", "", "a replacement password"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UnixMilli()
	if err := acme.Update("exp", Update{Expires: &past}); err != nil {
		t.Fatal(err)
	}
	for name, ses := range map[string]string{"disabled": disabled, "deleted": deleted, "password changed": changed, "expired": expired} {
		if _, ok := acme.Authenticate(ses); ok {
			t.Errorf("session survived: %s", name)
		}
	}
}

func TestChangeOwnPassword(t *testing.T) {
	root := New(filepath.Join(t.TempDir(), "c.json"), "super")
	acme := root.Tenant("acme")
	person(t, acme, "ana", "ana@plant.co", "correct horse battery", Viewer)
	if err := acme.ChangePassword("ana", "wrong", "a replacement password"); err == nil {
		t.Fatal("changed with the wrong current password")
	}
	if err := acme.ChangePassword("ana", "correct horse battery", "short"); err == nil {
		t.Fatal("accepted a too-short new password")
	}
	if err := acme.ChangePassword("ana", "correct horse battery", "a replacement password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := acme.AuthenticatePassword("ana@plant.co", "correct horse battery"); ok {
		t.Error("the old password still works")
	}
	if _, ok := acme.AuthenticatePassword("ana@plant.co", "a replacement password"); !ok {
		t.Error("the new password does not work")
	}
	// A device has no password to change.
	if _, err := acme.Add("pump", Device, []string{"*"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := acme.ChangePassword("pump", "x", "a replacement password"); err == nil {
		t.Error("changed a device's password")
	}
}

// Passwords survive a restart, and the stored file never holds one.
func TestPasswordsPersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	root := New(p, "super")
	person(t, root.Tenant("acme"), "ana", "ana@plant.co", "correct horse battery", Admin)

	again := New(p, "super")
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if id, ok := again.Tenant("acme").AuthenticatePassword("ana@plant.co", "correct horse battery"); !ok || id.ID != "ana" {
		t.Fatalf("after restart: %+v %v", id, ok)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "correct horse battery") {
		t.Fatal("the password is stored in plaintext")
	}
}
