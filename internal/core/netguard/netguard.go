// Package netguard tells the public addresses the server may reach on a
// user's word, as with webhooks and link previews, from those of its own
// network.
package netguard

import "net/netip"

// blocked are the ranges no fetch may reach besides those Public tests by
// kind: shared, benchmark, reserved, translated and tunneled addresses.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// Public reports whether addr is a public unicast address: not loopback,
// private, link-local (cloud metadata among them), multicast or reserved,
// nor one of those wrapped in IPv6.
func Public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	// 6to4 carries an IPv4 address that must be public too.
	if addr.Is6() && addr.As16()[0] == 0x20 && addr.As16()[1] == 0x02 {
		b := addr.As16()
		return Public(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return true
}
