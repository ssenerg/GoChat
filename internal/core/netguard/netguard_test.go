package netguard

import (
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "100.100.100.200", "0.0.0.0",
		"192.0.0.192", "::1", "::", "fe80::1", "fc00::1", "fd00:ec2::254", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"::a00:1", "64:ff9b::a00:1", "2002:a00:1::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2", "fec0::1",
		"224.0.0.1", "255.255.255.255", "198.18.0.1",
	} {
		if Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s passed as public", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "2002:5db8:d822::1", "::ffff:1.1.1.1"} {
		if !Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s did not pass as public", raw)
		}
	}
}
