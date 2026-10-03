package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestBlockedAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.10": true,
		"169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true, "::1": true, "fd00::1": true,
		"fe80::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "64:ff9b::a00:1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false, "52.95.110.1": false,
	} {
		if got := blocked(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s blocked=%v want %v", addr, got, want)
		}
	}
}

func TestGuardBlocksPrivateTargets(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	BlockPrivateNetworks(true)
	defer BlockPrivateNetworks(false)

	for _, spec := range []string{"webhook=" + srv.URL, "webhook=http://localhost:9/x", "slack=http://169.254.169.254/latest/meta-data",
		"email=smtp://10.0.0.5:25?from=a@b.c&to=d@e.f"} {
		if _, err := ParseTarget(spec); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s accepted: %v", spec, err)
		}
	}
	// A name that passes the configuration check but resolves to a
	// private address (DNS rebinding) is stopped when dialling.
	u, _ := url.Parse(srv.URL)
	tg := &Target{Kind: "webhook", URL: u, raw: srv.URL}
	n := New(Config{Targets: []*Target{tg}})
	res := n.SendTo(context.Background(), []*Target{tg}, Message{Status: "test", Title: "x"})
	if hit || res[tg.Redacted()] == "ok" {
		t.Fatalf("private webhook reached: %v", res)
	}
	mail, _ := url.Parse("smtp://127.0.0.1:2525?from=a@b.c&to=d@e.f")
	if err := sendEmail(mail, Message{Title: "x"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("private SMTP: %v", err)
	}

	// Off (single-tenant, on-premises): local targets work.
	BlockPrivateNetworks(false)
	n = New(Config{Targets: []*Target{tg}})
	if res := n.SendTo(context.Background(), []*Target{tg}, Message{Status: "test", Title: "x"}); !hit || res[tg.Redacted()] != "ok" {
		t.Fatalf("guard off: %v", res)
	}
}
