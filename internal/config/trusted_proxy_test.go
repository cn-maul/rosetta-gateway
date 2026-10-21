package config

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestTrustedProxyCIDRs_NormalizeAndDedup(t *testing.T) {
	c, err := Parse([]byte(`{
		"listen":"127.0.0.1:8666",
		"trusted_proxy_cidrs":["10.0.0.5/8","10.0.0.0/8","127.0.0.1","::1"]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantStrings := []string{"10.0.0.0/8", "127.0.0.1/32", "::1/128"}
	if !reflect.DeepEqual(c.TrustedProxyCIDRs, wantStrings) {
		t.Fatalf("TrustedProxyCIDRs=%v, want %v", c.TrustedProxyCIDRs, wantStrings)
	}
	wantPrefixes := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("::1/128"),
	}
	if got := c.TrustedProxyPrefixes(); !reflect.DeepEqual(got, wantPrefixes) {
		t.Fatalf("TrustedProxyPrefixes=%v, want %v", got, wantPrefixes)
	}
}

func TestTrustedProxyCIDRs_RejectInvalidEntries(t *testing.T) {
	for _, body := range []string{
		`{"listen":"127.0.0.1:8666","trusted_proxy_cidrs":[""]}`,
		`{"listen":"127.0.0.1:8666","trusted_proxy_cidrs":["proxy.local"]}`,
		`{"listen":"127.0.0.1:8666","trusted_proxy_cidrs":["10.0.0.0/99"]}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("非法 trusted_proxy_cidrs 应被拒绝: %s", body)
		}
	}
}
