# Upgrading to Networking v1

Networking v1 replaces the per-user-subnet model with two clean primitives:

- **Standalone VM** — per-VM Simple zone with PVE-native SNAT. Default for new
  provisions. No setup beyond `NIMBUS_STANDALONE_POOL_CIDR` (defaults to
  `10.128.0.0/9`). Each VM gets its own host-local `/24` — no cross-VM
  communication.
- **VPC** — VXLAN zone shared across cluster nodes plus a dedicated gateway LXC
  for NAT egress. VMs in the same VPC reach each other at L2 across nodes.

This is a **clean break**: there is no automatic migration. Existing VMs on the
legacy per-user-subnet model continue to run on their old NICs; new networks
are created under the new model.

## Pre-upgrade

1. **Drain or accept stranded legacy state.** Read the rest of this doc first
   so you understand what survives the upgrade and what doesn't.
2. **Reset legacy SDN state.** Click **Settings → Network → Reset SDN** in the
   running pre-upgrade Nimbus. This deletes the `user_subnets` rows and tears
   down the legacy zone in Proxmox. Your existing VMs keep their NICs (the
   reset refuses to run if any VMs are still attached — destroy those first if
   you want a fully clean cluster).
3. **Stop Nimbus.** `sudo systemctl stop nimbus`.

## Configure the new networking environment

Add the following to `/etc/nimbus/nimbus.env` (or `.env` in the working
directory).

### Standalone (always-on)

Optional, defaults work out of the box:

```bash
# Supernet for per-VM /24 carves. Default 10.128.0.0/9 (32K /24s).
# Override only if 10.128.0.0/9 conflicts with cluster-LAN routing.
NIMBUS_STANDALONE_POOL_CIDR=10.128.0.0/9
```

### VPCs (opt-in — admin must configure)

VPCs require all three of these to be set, otherwise the `/api/vpcs` route
doesn't mount and the VPC chip on the Provision page stays disabled:

```bash
# The PVE node where every VPC's gateway LXC will live. v1 limitation —
# HA across nodes is a future phase.
NIMBUS_NETWORK_NODE=pve-2

# Comma-separated IPv4 ranges for the gateway-LXC vmbr0-side IPs.
# Pick a window of your cluster LAN that doesn't overlap with VM /24s
# or anything the LAN's DHCP hands out.
NIMBUS_GATEWAY_LXC_IP_POOL=192.168.1.200-192.168.1.250

# Proxmox volid of an Alpine LXC template reachable on NETWORK_NODE.
# `pct create` for a quick test container can confirm the volid is
# valid before deploy.
NIMBUS_GATEWAY_LXC_TEMPLATE=local:vztmpl/alpine-3.20-default_20240908_amd64.tar.xz

# Optional: storage pool for the gateway LXC's rootfs. Default local-lvm.
NIMBUS_GATEWAY_LXC_STORAGE=local-lvm

# Optional: supernet for VPC /16 carves. Default 10.0.0.0/9 (32K /16s).
NIMBUS_VPC_POOL_CIDR=10.0.0.0/9
```

If you want to keep `nimbus install --upgrade` behavior, add these to the
installer's env-template too — `nimbus install --upgrade` replaces the binary
and restarts the systemd unit but doesn't touch your env file.

## Upgrade

1. Deploy the new binary: `sudo nimbus install --upgrade` (or replace
   `/usr/local/bin/nimbus` and `systemctl restart nimbus` manually).
2. Watch the startup log. The new lines you should see:

   ```
   gateway service: configured (network_node=pve-2)
   vpcmgr: enabled
   standalonenet: enabled (pool=10.128.0.0/9)
   ```

   If VPCs aren't configured you'll see:

   ```
   VPCs disabled — set NIMBUS_NETWORK_NODE + NIMBUS_GATEWAY_LXC_IP_POOL +
   NIMBUS_GATEWAY_LXC_TEMPLATE to enable
   ```

   Standalone still works in that mode.

## After upgrade

- New provisions default to **Standalone** mode unless the user picks **VPC**.
- The legacy per-user-subnet picker (`Existing` mode) still works but is
  hidden when no `user_subnets` rows exist for the caller.
- The `/subnets` page is preserved during the deprecation window so you can
  delete the old rows you don't want around. Once empty it's removed in v1.1.

## What gets dropped in v1.1

- The `user_subnets` table.
- The legacy `Existing`/`+ New subnet` picker modes on the Provision page.
- The `/api/subnets` routes.
- `internal/vnetmgr/` package.

There is no in-place migration from `user_subnets` → Standalone or VPC. If you
have legacy subnets you want to preserve as VPCs, recreate them by hand:
`POST /api/vpcs` with the same name, then re-provision the VMs you want as
members.

## Troubleshooting

- **"VPCs disabled"** at startup → confirm all three of `NIMBUS_NETWORK_NODE`,
  `NIMBUS_GATEWAY_LXC_IP_POOL`, and `NIMBUS_GATEWAY_LXC_TEMPLATE` are set in
  `/etc/nimbus/nimbus.env`. The other two without the third disables VPCs
  silently.
- **VPC create returns 503 "no online cluster nodes"** → the Proxmox cluster
  is offline or the API token can't read `/cluster/status`. Same root cause as
  the legacy bootstrap warning.
- **Gateway IP pool exhausted** → either the configured range was too small or
  old gateway LXC IPs aren't being released. Check the `gateway_lxc_ips`
  table; widen the pool by editing `NIMBUS_GATEWAY_LXC_IP_POOL` and
  restarting (existing rows are not clobbered, new rows are appended).
- **NAT not working inside VPC member VMs** → SSH into the gateway LXC
  (`pct enter <vmid>` on the network node), check `iptables -t nat -L
  POSTROUTING -nv` shows the MASQUERADE rule. If not, the in-LXC bootstrap
  failed; logs are in `/var/log/messages` inside the LXC.
