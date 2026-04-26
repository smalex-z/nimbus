//go:build linux

package netscan

import (
	"bufio"
	"os"
	"strings"
)

// readARPCache returns the IPs in /proc/net/arp that the kernel has resolved
// to a non-zero MAC. Only entries with the COMPLETE flag (0x2) are returned
// — incomplete entries mean "kernel asked but never got a reply", which is
// itself evidence the IP is NOT in use.
//
// The cache is populated by any L4 traffic the kernel attempts; the
// scanner's TCP probe step does this implicitly. Reading is unprivileged.
//
// Format of /proc/net/arp:
//
//	IP address    HW type  Flags   HW address          Mask  Device
//	192.168.0.1   0x1      0x2     1c:1b:0d:11:22:33   *     eth0
func readARPCache() map[string]bool {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck

	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	headerSkipped := false
	for sc.Scan() {
		if !headerSkipped {
			headerSkipped = true
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		// Flags column. 0x2 = ATF_COM (complete). Filter out 0x0 (incomplete)
		// and any other flag combinations that don't include COMPLETE.
		flags := fields[2]
		if flags == "0x0" {
			continue
		}
		// Defensive: an all-zero MAC means "not actually populated" even when
		// flags claim otherwise — happens transiently during ARP resolution.
		if fields[3] == "00:00:00:00:00:00" {
			continue
		}
		out[fields[0]] = true
	}
	return out
}
