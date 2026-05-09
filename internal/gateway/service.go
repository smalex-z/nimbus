// Package gateway owns the per-VPC gateway LXC lifecycle. Each VPC
// gets a dedicated Debian LXC with two NICs:
//
//   - eth0 on vmbr0 (or whatever the operator points at) with an IP
//     from the Nimbus-managed gateway-IP pool. Default route lives
//     here; this is where outbound MASQUERADE happens.
//   - eth1 on the VPC's VNet, with the VPC subnet's gateway IP
//     (.1 of the /16). Member VMs route their outbound traffic
//     through this IP.
//
// Inside the LXC: ip_forward=1 + iptables MASQUERADE on eth0, both
// persisted via /etc/sysctl.d and a systemd unit so the LXC
// recovers correctly across reboots. The OS family is fixed to
// debian-12-standard — Nimbus's SSH bootstrap script assumes
// apt + iptables + systemd, so operator-tunable templates are a
// foot-gun, not flexibility. Aplinfo still picks the latest minor
// (12.7, 12.8, …) so we don't churn the const on every Debian point
// release.
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
	"strconv"
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
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
	NextVMID(ctx context.Context) (int, error)
	StorageHasFile(ctx context.Context, node, storage, contentType, filename string) (bool, error)
	ListAvailableLXCTemplates(ctx context.Context, node string) ([]proxmox.AplinfoTemplate, error)
	DownloadLXCTemplate(ctx context.Context, node, storage, templateName string) (string, error)
	ReloadNodeNetwork(ctx context.Context, node string) error
	GetNodeNetworkInterface(ctx context.Context, node, iface string) (*proxmox.NodeNetworkInterface, error)
}

// gatewayTemplateFamily is the LXC template family Nimbus uses for
// every gateway. Debian 12 is fixed in code on purpose: the SSH
// bootstrap script assumes apt + iptables + systemd, debian-12 is
// supported by every PVE 7.x/8.x release, and "operator picks the
// OS" is a foot-gun (Alpine/Fedora/etc. would silently fail at
// bootstrap). Aplinfo still resolves the family to the latest
// available minor (12.7-1, 12.8-1, …).
const gatewayTemplateFamily = "debian-12-standard"

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
	// IPPoolStart and IPPoolEnd bound the host-network IPv4 range
	// the gateway LXCs allocate eth0 from. Seeded into db.GatewayLXCIP
	// at startup; the pool grows by re-running with a wider range
	// (existing rows are preserved).
	IPPoolStart string
	IPPoolEnd   string
	// LXCTemplate is an unexported test seam — when set, the service
	// uses the given volid verbatim and skips aplinfo discovery /
	// download. Production code never sets this; the env var and
	// Settings UI that previously fed it have been removed because
	// the gateway OS is fixed (see gatewayTemplateFamily).
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

	// templateMu guards reads + writes of cfg.LXCTemplate. The
	// startup ensure runs in a goroutine (so the HTTP listener
	// binds immediately) while Provision lazy-retries the same
	// ensure on first VPC create — both paths can mutate the
	// volid concurrently.
	templateMu sync.Mutex

	// bootstrapFn is the post-start NAT setup hook. Defaults to
	// runSSHBootstrap (real SSH-into-LXC) in production; tests
	// install a no-op via SetBootstrapFn so they don't try to dial
	// a non-existent SSH server.
	bootstrapFn func(ctx context.Context, host string, key *EphemeralKeypair, script string) error
}

// SetBootstrapFn replaces the post-start NAT bootstrap. Tests
// install a no-op so they don't try to SSH into a non-existent
// container; production code paths leave the default.
func (s *Service) SetBootstrapFn(fn func(ctx context.Context, host string, key *EphemeralKeypair, script string) error) {
	s.bootstrapFn = fn
}

// New constructs the Service and seeds db.GatewayLXCIP from cfg.IPPool
// if it's not already populated. Idempotent — safe to call on every
// boot. Returns an error when required config is missing or the
// pool string can't be parsed.
//
// LXCTemplate is a test-only override: production callers leave it
// blank and the service auto-picks the latest debian-12-standard
// from PVE's aplinfo at first VPC create (or via EnsureDefaultTemplate
// called from main.go startup). The operator setup is two settings —
// network node + IP pool.
func New(px LXCClient, dbConn *gorm.DB, cfg Config) (*Service, error) {
	if cfg.NetworkNode == "" {
		return nil, errors.New("gateway: NetworkNode is required")
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
	s := &Service{px: px, db: dbConn, cfg: cfg, bootstrapFn: runSSHBootstrap}
	if err := s.seedPool(cfg.IPPoolStart, cfg.IPPoolEnd); err != nil {
		return nil, fmt.Errorf("gateway: seed ip pool: %w", err)
	}
	return s, nil
}

// templateStorage is where Nimbus puts auto-downloaded LXC templates.
// Hardcoded to `local` since that's the only PVE storage that
// reliably accepts vztmpl content on a stock install.
const templateStorage = "local"

// EnsureDefaultTemplate picks the latest debian-12-standard system
// template from PVE's aplinfo, downloads it to `local` on the network
// node if not already cached, and stores the resulting volid on
// cfg.LXCTemplate. Idempotent — repeated calls no-op once a
// template is in place. Serialized via templateMu so a concurrent
// startup goroutine + first-Provision lazy retry don't both fire
// the download and race the volid write.
//
// No-op when cfg.LXCTemplate is already set (test seam, or a prior
// successful ensure). Returns the volid that the service will use
// for new gateway LXCs.
func (s *Service) EnsureDefaultTemplate(ctx context.Context) (string, error) {
	s.templateMu.Lock()
	defer s.templateMu.Unlock()
	if s.cfg.LXCTemplate != "" {
		return s.cfg.LXCTemplate, nil
	}

	available, err := s.px.ListAvailableLXCTemplates(ctx, s.cfg.NetworkNode)
	if err != nil {
		return "", fmt.Errorf("list aplinfo templates on %s: %w", s.cfg.NetworkNode, err)
	}
	pick := pickGatewayTemplate(available)
	if pick == "" {
		return "", fmt.Errorf("gateway: no %s template found in PVE aplinfo on %s — try `pveam update` on the node", gatewayTemplateFamily, s.cfg.NetworkNode)
	}

	have, err := s.px.StorageHasFile(ctx, s.cfg.NetworkNode, templateStorage, "vztmpl", pick)
	if err != nil {
		return "", fmt.Errorf("check template presence: %w", err)
	}
	if !have {
		log.Printf("gateway: downloading LXC template %s to %s:vztmpl on %s", pick, templateStorage, s.cfg.NetworkNode)
		taskID, err := s.px.DownloadLXCTemplate(ctx, s.cfg.NetworkNode, templateStorage, pick)
		if err != nil {
			return "", fmt.Errorf("dispatch template download: %w", err)
		}
		if taskID != "" {
			if err := s.px.WaitForTask(ctx, s.cfg.NetworkNode, taskID, s.cfg.PollInterval); err != nil {
				return "", fmt.Errorf("wait template download: %w", err)
			}
		}
	}

	s.cfg.LXCTemplate = fmt.Sprintf("%s:vztmpl/%s", templateStorage, pick)
	log.Printf("gateway: default template ready: %s", s.cfg.LXCTemplate)
	return s.cfg.LXCTemplate, nil
}

// pickGatewayTemplate scans aplinfo and returns the lexicographically-
// greatest amd64 template name in gatewayTemplateFamily (e.g.
// `debian-12-standard_12.8-1_amd64.tar.zst` beats
// `debian-12-standard_12.7-1_amd64.tar.zst`). Returns "" when no
// template of the family is present in the cluster's aplinfo.
func pickGatewayTemplate(in []proxmox.AplinfoTemplate) string {
	var best string
	for _, t := range in {
		name := t.Template
		if !strings.HasPrefix(name, gatewayTemplateFamily) {
			continue
		}
		if !strings.Contains(name, "_amd64.tar.") {
			continue
		}
		if name > best {
			best = name
		}
	}
	return best
}

// LXCTemplate returns the volid the service will use for new
// gateway LXCs — the test-seam value or the auto-downloaded default
// once EnsureDefaultTemplate has run. Empty string means the
// background ensure hasn't completed yet.
func (s *Service) LXCTemplate() string {
	s.templateMu.Lock()
	defer s.templateMu.Unlock()
	return s.cfg.LXCTemplate
}

// Provision creates a gateway LXC for a VPC. Returns the LXC's PVE
// VMID + the node it lives on (always cfg.NetworkNode in v1). The
// vpc.CIDR's .1 becomes the eth1 IP — VPC members route through it.
func (s *Service) Provision(ctx context.Context, vpc *db.VPC) (int, string, error) {
	// Lazy-ensure the default template if startup's background
	// ensure didn't get to it (e.g. NetworkNode came online late,
	// aplinfo was momentarily unreachable, or this is the very
	// first VPC create). No-op when LXCTemplate is already cached.
	templateVolid, err := s.EnsureDefaultTemplate(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("ensure default lxc template: %w", err)
	}
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

	// Auto-detect the host network's prefix + gateway from the
	// network node's vmbr0 (or whatever HostBridge is set to). This
	// avoids the foot-gun where the operator's legacy VM_PREFIX_LEN
	// (often 24) is wrong for a /16 cluster LAN — the LXC's eth0
	// would get the right IP but the wrong subnet mask, and its
	// kernel would refuse to route to the gateway because it's
	// "outside" the bogus /24. cfg values are the fallback when the
	// API call fails.
	hostPrefix, hostGw := s.cfg.HostPrefixLen, s.cfg.HostGatewayIP
	if iface, err := s.px.GetNodeNetworkInterface(ctx, s.cfg.NetworkNode, s.cfg.HostBridge); err == nil {
		if p := prefixFromIface(iface); p > 0 {
			hostPrefix = p
		}
		if iface.Gateway != "" {
			hostGw = iface.Gateway
		}
	} else {
		log.Printf("gateway: could not resolve %s on %s (%v) — falling back to cfg HostPrefixLen=%d HostGatewayIP=%s",
			s.cfg.HostBridge, s.cfg.NetworkNode, err, s.cfg.HostPrefixLen, s.cfg.HostGatewayIP)
	}
	hostNet := proxmox.LXCNetSpec{
		Name:   "eth0",
		Bridge: s.cfg.HostBridge,
		IP:     fmt.Sprintf("%s/%d", gwIP, hostPrefix),
		Gw:     hostGw,
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
	// vpcmgr.ApplySDN dispatches reload tasks asynchronously — by
	// the time we get here, the VPC's VXLAN bridge may not be up on
	// our network node yet, and StartLXC would race the reload and
	// fail with "bridge ... does not exist". One synchronous reload
	// on the single node we're about to land on closes the race.
	if err := s.px.ReloadNodeNetwork(ctx, s.cfg.NetworkNode); err != nil {
		log.Printf("gateway: pre-create reload network on %s: %v (continuing — start may race)", s.cfg.NetworkNode, err)
	}

	// Mint a one-shot ed25519 keypair Nimbus uses to SSH into the
	// freshly-created container exactly once. PVE writes the public
	// half into /root/.ssh/authorized_keys at create time; the
	// private half lives only in this function's stack frame. After
	// bootstrap completes the keypair is discarded — there's no
	// long-lived SSH credential anywhere on disk.
	keypair, err := newEphemeralKeypair()
	if err != nil {
		return 0, "", fmt.Errorf("generate ssh keypair: %w", err)
	}

	createTask, err := s.px.CreateLXC(ctx, s.cfg.NetworkNode, proxmox.LXCCreateOpts{
		VMID:          vmid,
		OSTemplate:    templateVolid,
		Hostname:      hostname,
		Storage:       s.cfg.LXCStorage,
		RootDiskGiB:   2,
		MemoryMiB:     256,
		Cores:         1,
		Net0:          hostNet.String(),
		Net1:          vpcNet.String(),
		Unprivileged:  true,
		SSHPublicKeys: keypair.authorizedKey,
		// Features intentionally unset. PVE gates every flag except
		// `nesting` to root@pam — tokens can't set `keyctl=1`. The
		// default-feature unprivileged container has CAP_NET_ADMIN,
		// which is all iptables MASQUERADE needs.
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

	if err := s.bootstrapNAT(ctx, gwIP, keypair); err != nil {
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

// bootstrapNAT SSHes into the freshly-created container at its
// host-network IP and runs the NAT setup as root. Connects with the
// ephemeral keypair PVE injected at create time. Targets Debian
// standard LXC templates: apt-get, iptables-persistent, systemd.
//
// Network-up handling is critical: PVE returns from StartLXC as soon
// as the container's init has launched; sshd takes another ~5-15 s
// to start. dialSSHWithRetries inside runSSHBootstrap polls TCP/22
// until the handshake works, so we don't sleep here.
func (s *Service) bootstrapNAT(ctx context.Context, gwIP string, key *EphemeralKeypair) error {
	const script = `set -e
export DEBIAN_FRONTEND=noninteractive

# Persist ip_forward (also enable now via sysctl --system).
cat > /etc/sysctl.d/99-nimbus.conf <<'EOF'
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
EOF
sysctl --system >/dev/null

# iptables-persistent flushes/restores rules at boot via the
# netfilter-persistent service. Pre-seed answers so apt doesn't
# prompt for "save current rules?".
echo iptables-persistent iptables-persistent/autosave_v4 boolean true | debconf-set-selections
echo iptables-persistent iptables-persistent/autosave_v6 boolean true | debconf-set-selections
apt-get update -qq
apt-get install -y -qq iptables iptables-persistent

# Idempotent MASQUERADE: -C succeeds if the rule already exists, in
# which case -A is skipped. eth0 is the host-network NIC PVE assigned.
iptables -t nat -C POSTROUTING -o eth0 -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE

# Persist via netfilter-persistent.
mkdir -p /etc/iptables
iptables-save  > /etc/iptables/rules.v4
ip6tables-save > /etc/iptables/rules.v6
systemctl enable netfilter-persistent
systemctl restart netfilter-persistent
`
	sshCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := s.bootstrapFn(sshCtx, gwIP, key, script); err != nil {
		return fmt.Errorf("ssh bootstrap: %w", err)
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

// seedPool inserts every IP in [start, end] as a free row.
// Existing rows are left untouched so re-seeding (e.g. widening the
// range) doesn't reset allocated state. Empty start AND end is a
// no-op — admin hasn't configured the pool yet.
func (s *Service) seedPool(start, end string) error {
	start = strings.TrimSpace(start)
	end = strings.TrimSpace(end)
	if start == "" && end == "" {
		return nil
	}
	if start == "" || end == "" {
		return fmt.Errorf("ip pool: both start and end required (got start=%q end=%q)", start, end)
	}
	startU, err := parseIPv4(start)
	if err != nil {
		return fmt.Errorf("parse start %q: %w", start, err)
	}
	endU, err := parseIPv4(end)
	if err != nil {
		return fmt.Errorf("parse end %q: %w", end, err)
	}
	if endU < startU {
		return fmt.Errorf("ip pool: end %s is before start %s", end, start)
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

// prefixFromIface extracts an IPv4 prefix length from PVE's interface
// shape. Tries iface.CIDR first ("192.168.0.245/16" — preferred),
// then iface.Netmask which can be either a prefix string ("16") or
// a dotted-quad mask ("255.255.0.0"). Returns 0 if nothing parses.
func prefixFromIface(iface *proxmox.NodeNetworkInterface) int {
	if iface == nil {
		return 0
	}
	if iface.CIDR != "" {
		if i := strings.LastIndex(iface.CIDR, "/"); i >= 0 {
			if p, err := strconv.Atoi(iface.CIDR[i+1:]); err == nil && p >= 1 && p <= 32 {
				return p
			}
		}
	}
	if iface.Netmask != "" {
		// Numeric prefix form ("16")
		if p, err := strconv.Atoi(iface.Netmask); err == nil && p >= 1 && p <= 32 {
			return p
		}
		// Dotted-quad form ("255.255.0.0")
		if mask := net.IPMask(net.ParseIP(iface.Netmask).To4()); mask != nil {
			ones, bits := mask.Size()
			if bits == 32 && ones >= 1 {
				return ones
			}
		}
	}
	return 0
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
