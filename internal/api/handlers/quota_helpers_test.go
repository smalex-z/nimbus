package handlers_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// happyFakePVE returns a provision.ProxmoxClient that succeeds on every call.
// Modelled after the identical helper in provision_test — duplicated here
// because the integration tests live in handlers_test, a separate package.
func happyFakePVE(t *testing.T) provision.ProxmoxClient {
	t.Helper()
	return &fakePVE{}
}

type fakePVE struct {
	nextID int
}

func (f *fakePVE) GetNodes(_ context.Context) ([]proxmox.Node, error) {
	return []proxmox.Node{
		{Name: "alpha", Status: "online", Mem: 1 << 30, MaxMem: 16 << 30, CPU: 0.1, MaxCPU: 8},
	}, nil
}
func (f *fakePVE) GetClusterVMs(_ context.Context) ([]proxmox.ClusterVM, error) { return nil, nil }
func (f *fakePVE) GetClusterStorage(_ context.Context) ([]proxmox.ClusterStorage, error) {
	return nil, nil
}
func (f *fakePVE) GetVMConfig(_ context.Context, _ string, vmid int) (map[string]any, error) {
	return map[string]any{
		"tags":  "nimbus-baked-v1",
		"scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,size=10G", vmid),
	}, nil
}
func (f *fakePVE) TemplateExists(_ context.Context, _ string, _ int) (bool, error) {
	return true, nil
}
func (f *fakePVE) NextVMID(_ context.Context) (int, error) {
	f.nextID++
	return 200 + f.nextID, nil
}
func (f *fakePVE) CloneVM(_ context.Context, _, _ string, _, newVMID int, _ string) (string, error) {
	return fmt.Sprintf("UPID:alpha:00001234::qmclone:%d:root@pam:", newVMID), nil
}
func (f *fakePVE) WaitForTask(_ context.Context, _, _ string, _ time.Duration) error { return nil }
func (f *fakePVE) SetCloudInit(_ context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error {
	return nil
}
func (f *fakePVE) SetVMTags(_ context.Context, _ string, _ int, _ []string) error { return nil }
func (f *fakePVE) SetVMDescription(_ context.Context, _ string, _ int, _ string) error {
	return nil
}
func (f *fakePVE) ResizeDisk(_ context.Context, _ string, _ int, _, _ string) error { return nil }
func (f *fakePVE) StartVM(_ context.Context, _ string, _ int) (string, error)       { return "", nil }
func (f *fakePVE) StopVM(_ context.Context, _ string, _ int) (string, error)        { return "", nil }
func (f *fakePVE) ShutdownVM(_ context.Context, _ string, _ int) (string, error)    { return "", nil }
func (f *fakePVE) RebootVM(_ context.Context, _ string, _ int) (string, error)      { return "", nil }
func (f *fakePVE) DestroyVM(_ context.Context, _ string, _ int) (string, error)     { return "", nil }
func (f *fakePVE) GetAgentInterfaces(_ context.Context, _ string, _ int) ([]proxmox.NetworkInterface, error) {
	return []proxmox.NetworkInterface{
		{Name: "ens18", IPAddresses: []proxmox.IPAddress{{IPAddress: "10.0.0.1", IPAddressType: "ipv4"}}},
	}, nil
}
func (f *fakePVE) MigrateVM(_ context.Context, _ string, _ int, _ string, _ bool) (string, error) {
	return "", nil
}
func (f *fakePVE) SetVMNetwork(_ context.Context, _ string, _ int, _, _ string) error { return nil }
func (f *fakePVE) AgentRun(_ context.Context, _ string, _ int, _ []string, _ string, _ time.Duration) (*proxmox.AgentExecStatus, error) {
	return &proxmox.AgentExecStatus{Exited: 1, ExitCode: 0}, nil
}
func (f *fakePVE) UploadFile(_ context.Context, _, _, _, _ string, _ []byte) error { return nil }
func (f *fakePVE) DeleteStorageVolume(_ context.Context, _, _ string) error        { return nil }
func (f *fakePVE) AttachCDROM(_ context.Context, _ string, _ int, _, _ string) error {
	return nil
}
func (f *fakePVE) AttachCloudInitDrive(_ context.Context, _ string, _ int, _, _ string) error {
	return nil
}
func (f *fakePVE) DetachDrive(_ context.Context, _ string, _ int, _ string) error { return nil }
