package provision

import (
	"context"
	"errors"
	"fmt"
	"log"

	"nimbus/internal/db"
	"nimbus/internal/ippool"
	"nimbus/internal/proxmox"
)

// NetworkOpReport summarizes a renumber or force-gateway batch run. Updated is
// the count of VMs that were successfully reconfigured (and rebooted, if
// applicable). Failures carries one entry per VM that errored — the rest of
// the batch continues so a single broken VM doesn't block the operation.
type NetworkOpReport struct {
	Updated  int
	Failures []NetworkOpFailure
}

// NetworkOpFailure pairs a VM identity with the error that aborted its update.
type NetworkOpFailure struct {
	VMRowID  uint
	VMID     int
	Hostname string
	Err      string
}

// managedVMsForNetworkOp returns the VMs the network ops should touch:
// non-soft-deleted rows with a real Proxmox VMID and node assignment, in
// status that implies the VM exists on Proxmox (excludes "creating"/"failed").
func (s *Service) managedVMsForNetworkOp(ctx context.Context) ([]db.VM, error) {
	var vms []db.VM
	if err := s.db.WithContext(ctx).
		Where("vmid > 0 AND node <> '' AND status NOT IN ?", []string{"creating", "failed"}).
		Order("id ASC").
		Find(&vms).Error; err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	return vms, nil
}

// ForceGatewayUpdate rewrites every managed VM's cloud-init ipconfig0 to use
// the supplied gateway, keeping each VM's existing IP, then reboots the VM so
// the new gateway takes effect. Disruptive — every VM bounces. Caller must
// confirm with the operator.
//
// Per-VM failures are collected, not returned, so the batch keeps going. A
// non-nil error from this method only indicates a setup failure (DB list,
// invalid gateway).
func (s *Service) ForceGatewayUpdate(ctx context.Context, gateway string) (NetworkOpReport, error) {
	if gateway == "" {
		return NetworkOpReport{}, errors.New("gateway is required")
	}
	vms, err := s.managedVMsForNetworkOp(ctx)
	if err != nil {
		return NetworkOpReport{}, err
	}
	rep := NetworkOpReport{}
	for _, vm := range vms {
		if vm.IP == "" {
			rep.Failures = append(rep.Failures, NetworkOpFailure{
				VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
				Err: "vm has no recorded IP",
			})
			continue
		}
		ipconfig := fmt.Sprintf("ip=%s/24,gw=%s", vm.IP, gateway)
		if err := s.px.SetCloudInit(ctx, vm.Node, vm.VMID, proxmox.CloudInitConfig{
			IPConfig0: ipconfig,
		}); err != nil {
			log.Printf("force-gateway: set cloud-init vmid=%d on %s: %v (skipping)", vm.VMID, vm.Node, err)
			rep.Failures = append(rep.Failures, NetworkOpFailure{
				VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
				Err: "set cloud-init: " + err.Error(),
			})
			continue
		}
		if err := s.rebootIfRunning(ctx, vm); err != nil {
			log.Printf("force-gateway: reboot vmid=%d on %s: %v (config saved, will apply on next boot)", vm.VMID, vm.Node, err)
			rep.Failures = append(rep.Failures, NetworkOpFailure{
				VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
				Err: "config saved but reboot failed: " + err.Error(),
			})
			continue
		}
		rep.Updated++
	}
	return rep, nil
}

// RenumberAllVMs reassigns every managed VM to a fresh IP from the current
// pool, updating cloud-init and rebooting each VM in turn. The new pool must
// already be saved (Reseed run) and have at least len(vms) free addresses.
//
// On success, each VM's old IP is released back to the pool (may then be
// pruned if outside the new range) and the vms.ip column is updated.
//
// Disruptive — every VM bounces. Per-VM failures roll back that one VM's
// reservation but don't abort the batch.
func (s *Service) RenumberAllVMs(ctx context.Context, gateway string) (NetworkOpReport, error) {
	if gateway == "" {
		return NetworkOpReport{}, errors.New("gateway is required")
	}
	vms, err := s.managedVMsForNetworkOp(ctx)
	if err != nil {
		return NetworkOpReport{}, err
	}
	free, err := s.pool.CountFree(ctx)
	if err != nil {
		return NetworkOpReport{}, fmt.Errorf("count free: %w", err)
	}
	if free < len(vms) {
		return NetworkOpReport{}, fmt.Errorf("pool has %d free addresses, need %d to renumber every VM", free, len(vms))
	}

	rep := NetworkOpReport{}
	for _, vm := range vms {
		newIP, err := s.pool.Reserve(ctx, vm.Hostname)
		if err != nil {
			if errors.Is(err, ippool.ErrPoolExhausted) {
				rep.Failures = append(rep.Failures, NetworkOpFailure{
					VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
					Err: "pool exhausted mid-renumber",
				})
				return rep, fmt.Errorf("pool exhausted after renumbering %d/%d", rep.Updated, len(vms))
			}
			rep.Failures = append(rep.Failures, NetworkOpFailure{
				VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
				Err: "reserve new ip: " + err.Error(),
			})
			continue
		}

		ipconfig := fmt.Sprintf("ip=%s/24,gw=%s", newIP, gateway)
		if err := s.px.SetCloudInit(ctx, vm.Node, vm.VMID, proxmox.CloudInitConfig{
			IPConfig0: ipconfig,
		}); err != nil {
			_ = s.pool.Release(ctx, newIP)
			log.Printf("renumber: set cloud-init vmid=%d on %s: %v (rolled back reservation)", vm.VMID, vm.Node, err)
			rep.Failures = append(rep.Failures, NetworkOpFailure{
				VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
				Err: "set cloud-init: " + err.Error(),
			})
			continue
		}

		if err := s.rebootIfRunning(ctx, vm); err != nil {
			log.Printf("renumber: reboot vmid=%d on %s: %v (config saved, IP not yet active)", vm.VMID, vm.Node, err)
			// Don't roll back — the cloud-init drive is already updated.
			// Mark the new IP allocated and update DB so the operator's
			// view is consistent; the VM picks up the new IP on next boot.
		}

		if err := s.pool.MarkAllocated(ctx, newIP, vm.VMID); err != nil {
			log.Printf("renumber: mark allocated %s vmid=%d: %v", newIP, vm.VMID, err)
		}
		oldIP := vm.IP
		if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).Update("ip", newIP).Error; err != nil {
			log.Printf("renumber: update vms.ip row=%d new=%s: %v", vm.ID, newIP, err)
			rep.Failures = append(rep.Failures, NetworkOpFailure{
				VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
				Err: "update vm record: " + err.Error(),
			})
			continue
		}
		if oldIP != "" {
			if err := s.pool.Release(ctx, oldIP); err != nil {
				log.Printf("renumber: release old ip %s vmid=%d: %v", oldIP, vm.VMID, err)
			}
		}
		rep.Updated++
	}
	return rep, nil
}

// rebootIfRunning issues a Proxmox reboot only when the local DB believes the
// VM is running. Avoids returning errors for stopped VMs (which would be
// expected — Proxmox refuses to reboot a stopped VM, but the cloud-init
// change still applies on next start).
func (s *Service) rebootIfRunning(ctx context.Context, vm db.VM) error {
	if vm.Status != "running" {
		return nil
	}
	taskID, err := s.px.RebootVM(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if taskID != "" {
		if err := s.px.WaitForTask(ctx, vm.Node, taskID, s.cfg.PollInterval); err != nil {
			return fmt.Errorf("reboot task: %w", err)
		}
	}
	return nil
}
