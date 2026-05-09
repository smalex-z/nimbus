package db

import (
	"fmt"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB wraps a GORM database connection.
type DB struct {
	*gorm.DB
}

// New opens a SQLite database at the given path and auto-migrates the supplied
// model types. SQLite's single-writer constraint is enforced by capping the
// connection pool at 1.
//
// Each package owning models passes its own pointer types so that there is no
// import cycle between the db layer and feature packages.
func New(path string, models ...any) (*DB, error) {
	gormDB, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get underlying sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(1) // SQLite supports one writer at a time
	sqlDB.SetMaxIdleConns(1)

	if err := gormDB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if len(models) > 0 {
		if err := gormDB.AutoMigrate(models...); err != nil {
			return nil, fmt.Errorf("auto-migrate: %w", err)
		}
	}

	// Partial unique indexes for soft-deletable models. GORM struct
	// tags can't express `WHERE deleted_at IS NULL`, but the
	// reconciler soft-deletes vm rows on purpose (preserve history,
	// allow recovery), and a plain unique index would keep tombstoned
	// vmids/hostnames "taken" forever — blocking VMID reuse after
	// Proxmox recycles the slot. Drop any AutoMigrate-created plain
	// unique indexes first, then create the partial ones.
	//
	// Per-table guard: tests sometimes open a DB without the vms
	// model (only the slice of tables they care about). Skip the
	// migration entirely when the parent table isn't present.
	if gormDB.Migrator().HasTable(&VM{}) {
		// idx_vms_vm_id (not _vmid) is what GORM auto-named the
		// unique index — VMID's camelCase splits to v_m_i_d-ish per
		// the CLAUDE.md naming-convention gotcha. Drop both spellings
		// to be safe across whatever historical builds put on disk.
		migrations := []string{
			`DROP INDEX IF EXISTS idx_vms_vm_id`,
			`DROP INDEX IF EXISTS idx_vms_vmid`,
			`DROP INDEX IF EXISTS idx_vms_hostname`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_vms_vmid_alive ON vms(vmid) WHERE deleted_at IS NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_vms_hostname_alive ON vms(hostname) WHERE deleted_at IS NULL`,
		}
		for _, stmt := range migrations {
			if err := gormDB.Exec(stmt).Error; err != nil {
				return nil, fmt.Errorf("post-migrate %q: %w", stmt, err)
			}
		}
	}

	// Same partial-unique pattern for the per-VM network tables. PVE
	// recycles vmids handed out by /cluster/nextid, so any unique
	// index on vm_id that doesn't filter on deleted_at would let a
	// tombstone block the next allocation that draws the same id.
	if gormDB.Migrator().HasTable(&VPCMembership{}) {
		migrations := []string{
			`DROP INDEX IF EXISTS idx_vpc_memberships_vm_id`,
			`DROP INDEX IF EXISTS idx_vpcmem_vpc_vm`,
			`DROP INDEX IF EXISTS idx_vpcmem_vpc_ip`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_vpcmem_vm_id_alive ON vpc_memberships(vm_id) WHERE deleted_at IS NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_vpcmem_vpc_vm_alive ON vpc_memberships(vpc_id, vm_id) WHERE deleted_at IS NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_vpcmem_vpc_ip_alive ON vpc_memberships(vpc_id, vm_ip) WHERE deleted_at IS NULL`,
		}
		for _, stmt := range migrations {
			if err := gormDB.Exec(stmt).Error; err != nil {
				return nil, fmt.Errorf("post-migrate %q: %w", stmt, err)
			}
		}
	}
	if gormDB.Migrator().HasTable(&StandaloneVMNetwork{}) {
		migrations := []string{
			`DROP INDEX IF EXISTS idx_standalone_vm_networks_vm_id`,
			`DROP INDEX IF EXISTS idx_standalone_vm_networks_zone_name`,
			`DROP INDEX IF EXISTS idx_standalone_vm_networks_vnet_name`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_standalone_vm_id_alive ON standalone_vm_networks(vm_id) WHERE deleted_at IS NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_standalone_zone_name_alive ON standalone_vm_networks(zone_name) WHERE deleted_at IS NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_standalone_vnet_name_alive ON standalone_vm_networks(vnet_name) WHERE deleted_at IS NULL`,
		}
		for _, stmt := range migrations {
			if err := gormDB.Exec(stmt).Error; err != nil {
				return nil, fmt.Errorf("post-migrate %q: %w", stmt, err)
			}
		}
	}

	return &DB{gormDB}, nil
}
