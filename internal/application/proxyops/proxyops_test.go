package proxyops

import "testing"

func TestWorkloadName(t *testing.T) {
	for digest, want := range map[string]string{
		"hmac-sha256:3f9a2c1b7e00": "shop-web-3f9a2c",
		"sha256:8b1d04aa":          "shop-web-8b1d04",
	} {
		if got := WorkloadName("shop", "web", digest); got != want {
			t.Errorf("WorkloadName(%q) = %q, want %q", digest, got, want)
		}
	}
}
