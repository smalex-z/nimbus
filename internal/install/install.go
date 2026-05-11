// Package install implements the `nimbus install` subcommand.
// It copies the running binary to /opt/nimbus, creates a dedicated service
// user, writes the systemd unit, enables the service, and writes a sudoers
// rule so future upgrades don't require a password.
//
// Configuration is NOT collected here — the server starts in setup mode and
// the web wizard at http://<host>:8080 handles configuration.
package install

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

const (
	installDir  = "/opt/nimbus"
	dataDir     = "/var/lib/nimbus"
	envDir      = "/etc/nimbus"
	envFile     = "/etc/nimbus/nimbus.env"
	serviceFile = "/etc/systemd/system/nimbus.service"
	sudoersFile = "/etc/sudoers.d/nimbus"
	serviceUser = "nimbus"
	appName     = "nimbus"

	// Cloud-tunnel bootstrap helper. The main nimbus.service unit is
	// hardened (NoNewPrivileges, ProtectSystem=strict) which prevents
	// it from running `apt install rathole` and dropping a systemd
	// unit on the host. The helper service runs UNHARDENED, as root,
	// in oneshot mode — fired by a path unit watching for a
	// trigger file the main service drops.
	gopherHelperPath    = "/etc/systemd/system/nimbus-gopher-bootstrap.path"
	gopherHelperService = "/etc/systemd/system/nimbus-gopher-bootstrap.service"
	gopherHelperUnit    = "nimbus-gopher-bootstrap.path"
)

// Run executes the install subcommand. If not running as root it re-execs
// itself via sudo (which may be passwordless after the first install
// thanks to the sudoers rule written below).
//
// With --upgrade, only the binary is replaced and the service restarted;
// env file, systemd unit, sudoers rule, and service user are left alone.
func Run(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	upgrade := fs.Bool("upgrade", false, "replace binary and restart service; leave config/systemd/sudoers untouched")
	unitsOnly := fs.Bool("units-only", false, "(re)write systemd units only — no binary copy or service restart. Used by reinstall.sh after a hot-swap to land new units without conflicting with the running binary.")
	_ = fs.Parse(args)

	if runtime.GOOS != "linux" {
		fatalf("nimbus install only supports Linux")
	}

	if os.Getuid() != 0 {
		selfExecWithSudo()
		return
	}

	if *unitsOnly {
		fmt.Println("\nNimbus Units-Only Refresh")
		fmt.Println("────────────────────────────────────────────────")
		// Re-writes systemd unit files in place. Skips installBinary
		// (which would 'text file busy' against the running process)
		// and skips restartService (caller controls lifecycle).
		// Idempotent.
		step("Writing systemd unit", writeServiceUnit)
		step("Writing gopher-bootstrap helper unit", writeGopherHelperUnit)
		// Enable the helper path unit so it's active after this call
		// returns. Idempotent — already-enabled is a no-op.
		_ = exec.Command("systemctl", "enable", gopherHelperUnit).Run()
		_ = exec.Command("systemctl", "start", gopherHelperUnit).Run()
		fmt.Printf("\n✅ Nimbus units refreshed.\n\n")
		return
	}

	if *upgrade {
		fmt.Println("\nNimbus Upgrade")
		fmt.Println("────────────────────────────────────────────────")
		step("Installing binary", installBinary)
		// Idempotent — re-writes the file with the current content
		// even when present, so an upgrade picks up unit-file
		// changes (e.g. the gopher-bootstrap helper itself, when
		// the unit definition shifts in a future release).
		step("Writing gopher-bootstrap helper unit", writeGopherHelperUnit)
		step("Restarting service", startService)
		fmt.Printf("\n✅ Nimbus upgraded.\n\n")
		return
	}

	// If a previous install left state behind (binary + systemd unit
	// already present), treat bare `install` as upgrade. Users
	// naturally re-run `install` for a refresh; making them remember
	// `--upgrade` is friction with no benefit. The upgrade path is a
	// strict subset of the install steps (no user/env/sudoers churn)
	// so this is always safe.
	if alreadyInstalled() {
		fmt.Println("\nNimbus Upgrade (detected existing install)")
		fmt.Println("────────────────────────────────────────────────")
		step("Installing binary", installBinary)
		step("Writing gopher-bootstrap helper unit", writeGopherHelperUnit)
		step("Restarting service", startService)
		fmt.Printf("\n✅ Nimbus upgraded.\n\n")
		return
	}

	fmt.Println("\nNimbus Installer")
	fmt.Println("────────────────────────────────────────────────")
	step("Creating directories", createDirs)
	step("Creating service user", createUser)
	step("Installing binary", installBinary)
	step("Writing env file stub", writeEnvStub)
	step("Writing systemd unit", writeServiceUnit)
	step("Writing gopher-bootstrap helper unit", writeGopherHelperUnit)
	step("Writing sudoers rule", writeSudoers)
	step("Enabling & starting service", startService)

	ip := primaryIP()
	fmt.Printf("\n✅ Nimbus installed.\n")
	fmt.Printf("   Open http://%s:8080 to complete setup.\n\n", ip)
}

// alreadyInstalled returns true when the install location holds a
// nimbus binary AND the systemd unit file exists. Used to auto-route
// bare `install` to the upgrade path so users don't have to remember
// `--upgrade` when iterating on a running host.
func alreadyInstalled() bool {
	if _, err := os.Stat(filepath.Join(installDir, appName)); err != nil {
		return false
	}
	if _, err := os.Stat(serviceFile); err != nil {
		return false
	}
	return true
}

func selfExecWithSudo() {
	exe, err := os.Executable()
	if err != nil {
		fatalf("cannot determine executable path: %v", err)
	}
	if abs, err := filepath.EvalSymlinks(exe); err == nil {
		exe = abs
	}
	args := append([]string{"sudo", exe}, os.Args[1:]...)
	if err := syscall.Exec("/usr/bin/sudo", args, os.Environ()); err != nil {
		fatalf("re-exec with sudo failed\nTry: sudo %s install\n%v", exe, err)
	}
}

func step(label string, fn func() error) {
	fmt.Printf("  → %-36s", label+"...")
	if err := fn(); err != nil {
		fmt.Printf("✗\n\nError: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func createDirs() error {
	for _, p := range []string{installDir, dataDir, envDir} {
		if err := os.MkdirAll(p, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", p, err)
		}
	}
	return nil
}

func createUser() error {
	if exec.Command("id", serviceUser).Run() != nil {
		if err := exec.Command("useradd", "-r", "-s", "/usr/sbin/nologin", "-d", dataDir, serviceUser).Run(); err != nil {
			return err
		}
	}
	// www-data group owns /etc/pve (Proxmox cluster filesystem) — membership
	// lets Nimbus read corosync.conf for cluster node discovery.
	_ = exec.Command("usermod", "-a", "-G", "www-data", serviceUser).Run()
	return nil
}

func installBinary() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	if abs, err := filepath.EvalSymlinks(exe); err == nil {
		exe = abs
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return fmt.Errorf("read binary: %w", err)
	}
	dest := filepath.Join(installDir, appName)
	// Write to a sibling temp file, then rename atomically. The
	// direct os.WriteFile(dest, ...) path fails with ETXTBSY ("text
	// file busy") whenever the destination is the currently-running
	// binary (the kernel blocks open-for-write on a live exe). A
	// rename works because the kernel keeps the OLD inode mmap'd
	// for the running process while the path now points at the new
	// inode — same trick `cp --remove-destination` uses.
	tmp := dest + ".new"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return fmt.Errorf("write tmp binary: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("swap binary into place: %w", err)
	}
	_ = exec.Command("chown", "-R", serviceUser+":"+serviceUser, dataDir).Run()
	return nil
}

func writeEnvStub() error {
	if _, err := os.Stat(envFile); err == nil {
		return nil // preserve existing config on reinstall/upgrade
	}
	stub := "# Nimbus configuration — complete setup at http://<host>:8080\n"
	if err := os.WriteFile(envFile, []byte(stub), 0600); err != nil {
		return err
	}
	// The nimbus service user must be able to write this file so the web
	// wizard can save configuration without root.
	return exec.Command("chown", serviceUser+":"+serviceUser, envDir, envFile).Run()
}

// serviceUnit is embedded as a constant so the binary carries the unit file
// without needing any external templates.
const serviceUnit = `[Unit]
Description=Nimbus VM provisioning portal
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nimbus
WorkingDirectory=/var/lib/nimbus
EnvironmentFile=-/etc/nimbus/nimbus.env
ExecStart=/opt/nimbus/nimbus
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/nimbus /etc/nimbus

[Install]
WantedBy=multi-user.target
`

func writeServiceUnit() error {
	if err := os.WriteFile(serviceFile, []byte(serviceUnit), 0644); err != nil {
		return err
	}
	return exec.Command("systemctl", "daemon-reload").Run()
}

// sudoersRule allows the sudo group to run `nimbus install` without a
// password prompt so upgrades and reconfigures don't need manual root.
const sudoersRule = `# Nimbus: passwordless install/upgrade for sudo group members.
%sudo ALL=(root) NOPASSWD: /opt/nimbus/nimbus install
`

func writeSudoers() error {
	return os.WriteFile(sudoersFile, []byte(sudoersRule), 0440)
}

// gopherHelperPathUnit watches /var/lib/nimbus/bootstrap-trigger and
// fires the helper service when the file appears. The main nimbus
// service drops the trigger; the helper service consumes (deletes)
// it after running so subsequent invocations re-fire cleanly.
const gopherHelperPathUnit = `[Unit]
Description=Watch for Nimbus gopher-bootstrap trigger
After=network-online.target

[Path]
PathExists=/var/lib/nimbus/bootstrap-trigger

[Install]
WantedBy=multi-user.target
`

// gopherHelperServiceUnit runs the actual install (curl | bash from
// the Gopher gateway). Deliberately UNHARDENED — apt and systemd
// drops need real root + filesystem write access. The trigger file
// is the only privilege boundary; nimbus.service can write into
// /var/lib/nimbus, that's it.
//
// Reads /var/lib/nimbus/bootstrap-pending.json (URL + machine_name),
// writes /var/lib/nimbus/bootstrap-result.json on completion. The
// nimbus subcommand handles all of that — see cmd/server/main.go's
// runGopherBootstrap.
const gopherHelperServiceUnit = `[Unit]
Description=Run Nimbus gopher-bootstrap (rathole install)
After=network-online.target

[Service]
Type=oneshot
ExecStart=/opt/nimbus/nimbus gopher-bootstrap
# Clean up the trigger file so the path unit re-fires on next touch
# rather than retriggering on metadata changes from the helper.
ExecStartPost=-/bin/rm -f /var/lib/nimbus/bootstrap-trigger
`

func writeGopherHelperUnit() error {
	if err := os.WriteFile(gopherHelperPath, []byte(gopherHelperPathUnit), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(gopherHelperService, []byte(gopherHelperServiceUnit), 0644); err != nil {
		return err
	}
	return exec.Command("systemctl", "daemon-reload").Run()
}

func startService() error {
	_ = exec.Command("systemctl", "enable", appName).Run()
	// Enable + start the helper path unit so the trigger-file watcher
	// is live before any operator clicks "Bootstrap cloud tunnel."
	// Failing here doesn't block the main service install — the
	// Settings page surfaces "helper unit not found" if the helper
	// is missing at runtime.
	_ = exec.Command("systemctl", "enable", gopherHelperUnit).Run()
	_ = exec.Command("systemctl", "start", gopherHelperUnit).Run()
	return exec.Command("systemctl", "restart", appName).Run()
}

func primaryIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer conn.Close() //nolint:errcheck
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
