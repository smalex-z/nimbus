// Package install implements the `nimbus install` subcommand.
// It copies the running binary to /opt/nimbus, creates a dedicated service
// user, writes the systemd unit, enables the service, and writes a sudoers
// rule so future upgrades don't require a password.
//
// Configuration is NOT collected here — the server starts in setup mode and
// the web wizard at http://<host>:8080 handles configuration.
package install

import (
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
)

// Run executes the install subcommand. If not running as root it re-execs
// itself via sudo (which may be passwordless after the first install
// thanks to the sudoers rule written below).
func Run() {
	if runtime.GOOS != "linux" {
		fatalf("nimbus install only supports Linux")
	}

	if os.Getuid() != 0 {
		selfExecWithSudo()
		return
	}

	fmt.Println("\nNimbus Installer")
	fmt.Println("────────────────────────────────────────────────")
	step("Creating directories", createDirs)
	step("Creating service user", createUser)
	step("Installing binary", installBinary)
	step("Writing env file stub", writeEnvStub)
	step("Writing systemd unit", writeServiceUnit)
	step("Writing sudoers rule", writeSudoers)
	step("Enabling & starting service", startService)

	ip := primaryIP()
	fmt.Printf("\n✅ Nimbus installed.\n")
	fmt.Printf("   Open http://%s:8080 to complete setup.\n\n", ip)
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
	if exec.Command("id", serviceUser).Run() == nil {
		return nil // already exists
	}
	return exec.Command("useradd", "-r", "-s", "/usr/sbin/nologin", "-d", dataDir, serviceUser).Run()
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
	if err := os.WriteFile(dest, data, 0755); err != nil {
		return fmt.Errorf("write binary: %w", err)
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

func startService() error {
	_ = exec.Command("systemctl", "enable", appName).Run()
	return exec.Command("systemctl", "restart", appName).Run()
}

func primaryIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
