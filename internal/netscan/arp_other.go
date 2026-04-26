//go:build !linux

package netscan

// readARPCache is a no-op on non-Linux platforms. Macs and Windows expose
// the ARP table via different interfaces (`arp -a`, `GetIpNetTable2`); we
// don't bother because Nimbus's production deployment is Linux. Dev builds
// on macOS just lose the ARP-cache enhancement and fall back to TCP-probe
// only — which still catches every host that responds on a common port.
func readARPCache() map[string]bool { return nil }
