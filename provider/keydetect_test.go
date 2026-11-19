package provider

import "testing"

func TestAuthKeySuffix(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{"free key", "1234-abcd:fx", AuthKeySuffixFree},
		{"free key uppercase", "1234-ABCD:FX", AuthKeySuffixFree},
		{"deepl-pro key", "1234-abcd:dp", AuthKeySuffixDeepLPro},
		{"deepl-pro key with spaces", " 1234-abcd:dp ", AuthKeySuffixDeepLPro},
		{"paid key without suffix", "1234-abcd-efgh", ""},
		{"suffix inside key only", "dp-1234-abcd", ""},
		{"empty", "", ""},
		{"similar suffix", "1234:dp2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthKeySuffix(tc.key); got != tc.want {
				t.Fatalf("AuthKeySuffix(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestEndpointFromAuthKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		endpoint string
		provider string
		ok       bool
	}{
		{"deepl-pro suffix", "abc:dp", EndpointDeepLPro, "deepl", true},
		{"free suffix", "abc:fx", EndpointDeepLFree, "deepl", true},
		{"paid no suffix", "abc", EndpointDeepLOfficial, "deepl", true},
		{"empty key", "   ", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, providerName, ok := EndpointFromAuthKey(tc.key)
			if endpoint != tc.endpoint || providerName != tc.provider || ok != tc.ok {
				t.Fatalf("EndpointFromAuthKey(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.key, endpoint, providerName, ok, tc.endpoint, tc.provider, tc.ok)
			}
		})
	}
}

func TestIsAutoEndpoint(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"auto", true},
		{"AUTO", true},
		{" detect ", true},
		{"https://api.deepl.com", false},
		{"deepl-pro", false},
	} {
		if got := IsAutoEndpoint(tc.in); got != tc.want {
			t.Fatalf("IsAutoEndpoint(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
