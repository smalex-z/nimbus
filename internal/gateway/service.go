// Package gateway owns the per-VPC gateway LXC lifecycle. Each VPC
// gets a dedicated Alpine LXC with two NICs:
//
//   - eth0 on vmbr0 (or whatever the operator points at) with an IP
//     from the Nimbus-managed gateway-IP pool. Default route lives
//     here; this is where outbound MASQUERADE happens.
//   - eth1 on the VPC's VNet, with the VPC subnet's gateway IP
//     (.1 of the /16). Member VMs route their outbound traffic
//     through this IP.
//
// Inside the LXC: ip_forward=1 + iptables MASQUERADE on eth0. Both
// are persisted via /etc/sysctl.d and Alpine's iptables OpenRC
// service so the LXC recovers correctly across reboots.
//
// One designated network node holds every VPC's gateway LXC in v1
// (NIMBUS_NETWORK_NODE). HA-VRRP is a future phase. Inter-VM VPC
// traffic survives gateway outages (it's pure VXLAN); only egress
// breaks until the LXC restarts.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// LXCClient is the slice of *proxmox.Client this package uses.
type LXCClient interface {
	CreateLXC(ctx context.Context, node string, opts proxmox.LXCCreateOpts) (string, error)
	StartLXC(ctx context.Context, node string, vmid int) (string, error)
	StopLXC(ctx context.Context, node string, vmid int) (string, error)
	DestroyLXC(ctx context.Context, node string, vmid int) (string, error)
	LXCExecShell(ctx context.Context, node string, vmid int, script string) (*proxmox.LXCExecResult, error)
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
	NextVMID(ctx context.Context) (int, error)
}

// Config holds deployment-specific knobs for the gateway service.
type Config struct {
	// NetworkNode is the PVE node where every VPC gateway LXC lives.
	// Required — the package refuses to start when empty. v1 limitation;
	// future phase will spread gateways with VRRP.
	NetworkNode string
	// HostBridge is the LXC's eth0 bridge. Almost always "vmbr0";
	// override when the operator runs a custom management bridge.
	HostBridge string
	// HostGatewayIP is the LXC's default-route gateway on the host
	// network (i.e. the LAN's actual router). Inside the LXC this
	// becomes `route add default via <ip>`.
	HostGatewayIP string
	// HostPrefixLen is the netmask of the host network (e.g. 24 for
	// /24). Used as the prefix on the LXC's eth0 IP.
	HostPrefixLen int
	// IPPool is the comma-separated list of host-network IP ranges
	// (e.g. "192.168.1.200-192.168.1.250"). Seeded into
	// db.GatewayLXCIP at startup; gateway LXCs allocate from here.
	IPPool string
	// LXCTemplate is the Proxmox volid of an Alpine template
	// reachable on NetworkNode (e.g.
	// "local:vztmpl/alpine-3.20-default_20240908_amd64.tar.xz").
	// Required — Nimbus does not auto-download templates here.
	LXCTemplate string
	// LXCStorage is the storage pool for the gateway LXC's rootfs.
	// Default "local-lvm".
	LXCStorage string
	// PollInterval is the WaitForTask cadence. Default 1s.
	PollInterval time.Duration
}

// Service is the gateway-LXC manager. Methods are safe to call
// concurrently — IP allocation is atomic via SQLite and LXC
// creation is gated by the cluster-wide NextVMID lock.
type Service struct {
	px  LXCClient
	db  *gorm.DB
	cfg Config

	// allocMu serializes IP-pool reservation. SQLite already
	// serializes writes, but the read-then-update pattern below
	// needs a happy path that doesn't churn through retries on
	// race losses.
	allocMu sync.Mutex
}

// New constructs the Service and seeds db.GatewayLXCIP from cfg.IPPool
// if it's not already populated. Idempotent — safe to call on every
// boot. Returns an error when required config is missing or the
// pool string can't be parsed.
func New(px LXCClient, dbConn *gorm.DB, cfg Config) (*Service, error) {
	if cfg.NetworkNode == "" {
		return nil, errors.New("gateway: NetworkNode is required")
	}
	if cfg.LXCTemplate == "" {
		return nil, errors.New("gateway: LXCTemplate is required")
	}
	if cfg.HostBridge == "" {
		cfg.HostBridge = "vmbr0"
	}
	if cfg.LXCStorage == "" {
		cfg.LXCStorage = "local-lvm"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.HostPrefixLen == 0 {
		cfg.HostPrefixLen = 24
	}
	s := &Service{px: px, db: dbConn, cfg: cfg}
	if err := s.seedPool(cfg.IPPool); err != nil {
		return nil, fmt.Errorf("gateway: seed ip pool: %w", err)
	}
	return s, nil
}

// Provision creates a gateway LXC for a VPC. Returns the LXC's PVE
// VMID + the node it lives on (always cfg.NetworkNode in v1). The
// vpc.CIDR's .1 becomes the eth1 IP — VPC members route through it.
func (s *Service) Provision(ctx context.Context, vpc *db.VPC) (int, string, error) {
	gwIP, err := s.reserveHostIP(ctx, vpc.ID)
	if err != nil {
		return 0, "", fmt.Errorf("reserve host ip: %w", err)
	}
	released := false
	defer func() {
		if !released {
			_ = s.releaseHostIP(context.Background(), gwIP)
		}
	}()

	vpcGatewayIP, _, err := splitGatewayAndHost(vpc.CIDR)
	if err != nil {
		return 0, "", fmt.Errorf("derive vpc gateway: %w", err)
	}
	vpcPrefix, err := prefixLenOf(vpc.CIDR)
	if err != nil {
		return 0, "", err
	}

	vmid, err := s.px.NextVMID(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("nextvmid: %w", err)
	}

	hostNet := proxmox.LXCNetSpec{
		Name:   "eth0",
		Bridge: s.cfg.HostBridge,
		IP:     fmt.Sprintf("%s/%d", gwIP, s.cfg.HostPrefixLen),
		Gw:     s.cfg.HostGatewayIP,
	}
	vpcNet := proxmox.LXCNetSpec{
		Name:   "eth1",
		Bridge: vpc.VNetName,
		IP:     fmt.Sprintf("%s/%d", vpcGatewayIP, vpcPrefix),
	}

	hostname := fmt.Sprintf("nbu-gw-%s", vpc.ZoneName)
	if len(hostname) > 63 {
		hostname = hostname[:63]
	}
	createTask, err := s.px.CreateLXC(ctx, s.cfg.NetworkNode, proxmox.LXCCreateOpts{
		VMID:         vmid,
		OSTemplate:   s.cfg.LXCTemplate,
		Hostname:     hostname,
		Storage:      s.cfg.LXCStorage,
		RootDiskGiB:  1,
		MemoryMiB:    128,
		Cores:        1,
		Net0:         hostNet.String(),
		Net1:         vpcNet.String(),
		Unprivileged: true,
		Features:     "keyctl=1",
	})
	if err != nil {
		return 0, "", fmt.Errorf("create lxc: %w", err)
	}
	if err := s.px.WaitForTask(ctx, s.cfg.NetworkNode, createTask, s.cfg.PollInterval); err != nil {
		return 0, "", fmt.Errorf("wait create: %w", err)
	}
	cleanupVMID := vmid
	defer func() {
		if cleanupVMID == 0 {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if upid, err := s.px.StopLXC(cctx, s.cfg.NetworkNode, cleanupVMID); err == nil {
			_ = s.px.WaitForTask(cctx, s.cfg.NetworkNode, upid, s.cfg.PollInterval)
		}
		if upid, err := s.px.DestroyLXC(cctx, s.cfg.NetworkNode, cleanupVMID); err == nil {
			_ = s.px.WaitForTask(cctx, s.cfg.NetworkNode, upid, s.cfg.PollInterval)
		}
	}()

	startTask, err := s.px.StartLXC(ctx, s.cfg.NetworkNode, vmid)
	if err != nil {
		return 0, "", fmt.Errorf("start lxc: %w", err)
	}
	if startTask != "" {
		if err := s.px.WaitForTask(ctx, s.cfg.NetworkNode, startTask, s.cfg.PollInterval); err != nil {
			return 0, "", fmt.Errorf("wait start: %w", err)
		}
	}

	if err := s.bootstrapNAT(ctx, vmid); err != nil {
		return 0, "", fmt.Errorf("bootstrap nat: %w", err)
	}

	if err := s.markHostIPAllocated(ctx, gwIP, vpc.ID); err != nil {
		log.Printf("gateway: mark host ip %s allocated for vpc %d: %v", gwIP, vpc.ID, err)
	}
	released = true
	cleanupVMID = 0
	return vmid, s.cfg.NetworkNode, nil
}

// Destroy stops + destroys a VPC's gateway LXC and releases its
// host-network IP. Idempotent against missing PVE state — already
// gone is treated as success.
func (s *Service) Destroy(ctx context.Context, vpc *db.VPC) error {
	if vpc.GatewayLXCID != nil {
		vmid := *vpc.GatewayLXCID
		if upid, err := s.px.StopLXC(ctx, s.gatewayNode(vpc), vmid); err == nil {
			_ = s.px.WaitForTask(ctx, s.gatewayNode(vpc), upid, s.cfg.PollInterval)
		} else if !isAlreadyGone(err) {
			log.Printf("gateway: stop lxc %d on %s: %v (continuing)", vmid, s.gatewayNode(vpc), err)
		}
		if upid, err := s.px.DestroyLXC(ctx, s.gatewayNode(vpc), vmid); err != nil {
			if !isAlreadyGone(err) {
				return fmt.Errorf("destroy lxc %d: %w", vmid, err)
			}
		} else {
			_ = s.px.WaitForTask(ctx, s.gatewayNode(vpc), upid, s.cfg.PollInterval)
		}
	}
	// Release the host IP linked to this VPC, regardless of whether
	// we found one — pre-Provision failures may have reserved without
	// allocating, and the caller still wants the row freed.
	var rows []db.GatewayLXCIP
	if err := s.db.WithContext(ctx).Where("vpc_id = ?", vpc.ID).Find(&rows).Error; err != nil {
		return fmt.Errorf("lookup gateway ips for vpc %d: %w", vpc.ID, err)
	}
	for _, r := range rows {
		if err := s.releaseHostIP(ctx, r.IP); err != nil {
			log.Printf("gateway: release host ip %s: %v", r.IP, err)
		}
	}
	return nil
}

// bootstrapNAT runs the in-LXC commands that turn it into a NAT
// gateway. Persists across reboots via /etc/sysctl.d + the Alpine
// iptables OpenRC service. The script idempotently re-applies on
// repeated calls — no harm in running it twice during retry.
func (s *Service) bootstrapNAT(ctx context.Context, vmid int) error {
	const script = `set -e
# Wait briefly for the container's network to come up.
for i in 1 2 3 4 5; do
  ip a show eth0 | grep -q "inet " && break
  sleep 1
done

# Persist ip_forward.
echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/10-nimbus.conf
sysctl -w net.ipv4.ip_forward=1

# Install iptables if missing.
if ! command -v iptables >/dev/null 2>&1; then
  apk add --no-cache iptables
fi

# Idempotent MASQUERADE rule.
iptables -t nat -C POSTROUTING -o eth0 -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE

# Persist via OpenRC service so rules survive reboot.
mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules-save
rc-update add iptables default 2>/dev/null || true
rc-service iptables save 2>/dev/null || true
`
	res, err := s.px.LXCExecShell(ctx, s.cfg.NetworkNode, vmid, script)
	if err != nil {
		return fmt.Errorf("exec bootstrap: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("bootstrap exited %d: stderr=%s", res.ExitCode, res.ErrData)
	}
	return nil
}

// reserveHostIP atomically picks the lowest-numbered free host IP
// and stamps it for this VPC.
func (s *Service) reserveHostIP(ctx context.Context, vpcID uint) (string, error) {
	s.allocMu.Lock()
	defer s.allocMu.Unlock()
	var row db.GatewayLXCIP
	if err := s.db.WithContext(ctx).
		Where("status = ?", "free").
		Order("id ASC").
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", errors.New("gateway IP pool exhausted (configure NIMBUS_GATEWAY_LXC_IP_POOL with a larger range)")
		}
		return "", err
	}
	res := s.db.WithContext(ctx).Model(&row).
		Where("status = ?", "free").
		Updates(map[string]any{"status": "reserved", "vpc_id": vpcID})
	if res.Error != nil {
		return "", res.Error
	}
	if res.RowsAffected == 0 {
		// race lost; caller can retry
		return "", errors.New("gateway: lost race reserving ip — retry")
	}
	return row.IP, nil
}

func (s *Service) markHostIPAllocated(ctx context.Context, ip string, vpcID uint) error {
	res := s.db.WithContext(ctx).Model(&db.GatewayLXCIP{}).
		Where("ip = ? AND status = ?", ip, "reserved").
		Updates(map[string]any{"status": "allocated", "vpc_id": vpcID})
	if res.Error != nil {
		return res.Error
	}
	return nil
}

func (s *Service) releaseHostIP(ctx context.Context, ip string) error {
	res := s.db.WithContext(ctx).Model(&db.GatewayLXCIP{}).
		Where("ip = ?", ip).
		Updates(map[string]any{"status": "free", "vpc_id": nil})
	return res.Error
}

func (s *Service) gatewayNode(vpc *db.VPC) string {
	if vpc.GatewayNode != "" {
		return vpc.GatewayNode
	}
	return s.cfg.NetworkNode
}

// seedPool inserts every IP in the configured ranges as a free row.
// Existing rows are left untouched (so re-seeding doesn't reset
// allocated state). Pool format: "192.168.1.200-192.168.1.250" or
// comma-separated multiple ranges.
func (s *Service) seedPool(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil // no pool configured — admin must populate before VPCs work
	}
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.SplitN(raw, "-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid range %q (expected start-end)", raw)
		}
		start := strings.TrimSpace(parts[0])
		end := strings.TrimSpace(parts[1])
		startU, err := parseIPv4(start)
		if err != nil {
			return fmt.Errorf("parse start %q: %w", start, err)
		}
		endU, err := parseIPv4(end)
		if err != nil {
			return fmt.Errorf("parse end %q: %w", end, err)
		}
		if endU < startU {
			return fmt.Errorf("range %q: end before start", raw)
		}
		for u := startU; u <= endU; u++ {
			ip := uint32ToIPv4(u).String()
			row := db.GatewayLXCIP{IP: ip, Status: "free"}
			// FirstOrCreate by IP keeps the seed idempotent.
			if err := s.db.Where(&db.GatewayLXCIP{IP: ip}).
				Attrs(&db.GatewayLXCIP{Status: "free"}).
				FirstOrCreate(&row).Error; err != nil {
				return fmt.Errorf("seed row for %s: %w", ip, err)
			}
		}
	}
	return nil
}

// splitGatewayAndHost mirrors vpcmgr.splitGatewayAndHost — kept
// internal so the package has no dependency on vpcmgr.
func splitGatewayAndHost(cidr string) (gateway, host string, err error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	base := ipv4ToUint32(ipnet.IP.To4())
	return uint32ToIPv4(base + 1).String(), uint32ToIPv4(base + 10).String(), nil
}

func prefixLenOf(cidr string) (int, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	ones, _ := ipnet.Mask.Size()
	return ones, nil
}

func parseIPv4(s string) (uint32, error) {
	ip := net.ParseIP(strings.TrimSpace(s)).To4()
	if ip == nil {
		return 0, fmt.Errorf("not a valid IPv4 address: %s", s)
	}
	return ipv4ToUint32(ip), nil
}

func ipv4ToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIPv4(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).To4()
}

func isAlreadyGone(err error) bool {
	if errors.Is(err, proxmox.ErrNotFound) {
		return true
	}
	var httpErr *proxmox.HTTPError
	if errors.As(err, &httpErr) && strings.Contains(httpErr.Body, "does not exist") {
		return true
	}
	return false
}
