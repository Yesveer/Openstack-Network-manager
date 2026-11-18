package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/yesveer/openstack-net-attach/internal/config"
)

// Manager handles network interface configuration for hot-plugged OpenStack NICs.
type Manager struct {
	cfg         *config.Config
	logger      *slog.Logger
	configuring sync.Map // tracks interfaces currently being configured

	// Absolute paths of external tools, resolved once from trusted system
	// directories so that $PATH is never consulted.
	networkctlPath string
	dhclientPath   string
}

func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	return &Manager{
		cfg:            cfg,
		logger:         logger,
		networkctlPath: systemBinary("networkctl"),
		dhclientPath:   systemBinary("dhclient"),
	}
}

// trustedBinDirs are the only directories external tools are executed from.
var trustedBinDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// systemBinary returns the absolute path of name inside trustedBinDirs. If it
// is not found, it still returns an absolute path so exec fails with a clear
// "no such file" error instead of falling back to a $PATH lookup.
func systemBinary(name string) string {
	for _, dir := range trustedBinDirs {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return filepath.Join(trustedBinDirs[0], name)
}

// ifaceNameRe matches valid Linux interface names (IFNAMSIZ-1 = 15 chars).
// Names are used in file paths, .network file content and command arguments,
// so anything outside this conservative set is rejected.
var ifaceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

// ValidInterfaceName reports whether name is a safe Linux interface name.
func ValidInterfaceName(name string) bool {
	return ifaceNameRe.MatchString(name) && name != "." && name != ".."
}

// ShouldManage returns true if the daemon should manage this interface.
func (m *Manager) ShouldManage(name string) bool {
	if !ValidInterfaceName(name) {
		m.logger.Warn("ignoring interface with unsafe name", "interface", name)
		return false
	}
	for _, prefix := range m.cfg.ExcludePrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	for _, prefix := range m.cfg.IncludePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// HasIP returns true if the interface already has a non-link-local IPv4 address.
func (m *Manager) HasIP(name string) bool {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return false
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if addr.IP != nil && !addr.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// ConfigureInterface brings up a hot-plugged interface and configures DHCP on it.
// It is safe to call concurrently; duplicate calls for the same interface are deduplicated.
func (m *Manager) ConfigureInterface(ctx context.Context, name string) {
	if !ValidInterfaceName(name) {
		m.logger.Warn("refusing to configure interface with unsafe name", "interface", name)
		return
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		m.logger.Debug("interface vanished before configuration", "interface", name, "error", err)
		return
	}

	// A hot-plugged NIC first appears with a kernel name (eth0) and is renamed
	// by udev a few milliseconds later (ens7). Wait for udev to finish, then
	// continue with the final name so we never configure the transient one.
	m.waitUdevInitialized(ctx, link.Attrs().Index)
	if link, err = netlink.LinkByIndex(link.Attrs().Index); err != nil {
		m.logger.Debug("interface vanished before configuration", "interface", name, "error", err)
		return
	}
	if cur := link.Attrs().Name; cur != name {
		m.logger.Debug("interface was renamed by udev", "from", name, "to", cur)
		name = cur
		if !m.ShouldManage(name) {
			return
		}
	}
	mac := link.Attrs().HardwareAddr

	// Only one goroutine may configure a given NIC at a time. The MAC is the
	// key because the same NIC can be reported under two names (see above).
	key := name
	if len(mac) > 0 {
		key = mac.String()
	}
	if _, loaded := m.configuring.LoadOrStore(key, struct{}{}); loaded {
		m.logger.Debug("interface configuration already in progress", "interface", name)
		return
	}
	defer m.configuring.Delete(key)

	log := m.logger.With("interface", name, "mac", mac.String())

	if m.HasIP(name) {
		log.Debug("interface already has an IP address, skipping")
		return
	}

	if m.isNetworkdManaged(name, mac) {
		log.Debug("interface already has a networkd config file, skipping")
		return
	}

	log.Info("new interface detected, configuring")

	// Bring up the interface so carrier detection can proceed.
	if err := m.bringUp(name); err != nil {
		log.Warn("failed to set interface up", "error", err)
	}

	// Wait for layer-2 carrier before starting DHCP.
	if err := m.waitForCarrier(ctx, name); err != nil {
		log.Warn("carrier wait timed out, attempting DHCP anyway", "error", err)
	}

	// Configure DHCP using the configured backend with retries.
	var lastErr error
	for attempt := 1; attempt <= m.cfg.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return
		}

		log.Info("configuring DHCP", "attempt", attempt, "backend", m.cfg.Backend)

		switch m.cfg.Backend {
		case config.BackendNetworkd:
			lastErr = m.configureViaNetworkd(ctx, name, mac)
		default:
			lastErr = m.configureViaDHClient(ctx, name)
		}

		if lastErr == nil {
			log.Info("interface configured successfully")
			go func() {
				if err := m.waitAndSetupPolicyRouting(ctx, name); err != nil {
					log.Warn("policy routing setup failed", "error", err)
				}
			}()
			return
		}

		log.Warn("DHCP configuration failed", "attempt", attempt, "error", lastErr)

		if attempt < m.cfg.MaxRetries {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(m.cfg.RetryDelay) * time.Second):
			}
		}
	}

	log.Error("failed to configure interface after all attempts", "error", lastErr)
}

// IsManagedByUs returns true if this app previously configured the interface
// (i.e., a tracking file from a prior ConfigureInterface call exists).
// mac may be nil when unknown.
func (m *Manager) IsManagedByUs(name string, mac net.HardwareAddr) bool {
	switch m.cfg.Backend {
	case config.BackendNetworkd:
		return m.isNetworkdManaged(name, mac)
	default:
		_, err := os.Stat(m.dhclientLeaseFile(name))
		return err == nil
	}
}

// ReleaseInterface releases the DHCP lease and removes configuration for an interface.
// It only acts on interfaces that were configured by this app; others are ignored.
// mac is the NIC's hardware address as reported in the RTM_DELLINK message.
func (m *Manager) ReleaseInterface(ctx context.Context, name string, mac net.HardwareAddr) {
	if !ValidInterfaceName(name) {
		m.logger.Warn("refusing to release interface with unsafe name", "interface", name)
		return
	}

	log := m.logger.With("interface", name, "mac", mac.String())

	if !m.IsManagedByUs(name, mac) {
		log.Debug("interface was not configured by us, skipping release")
		return
	}

	log.Info("interface removed, releasing DHCP")

	m.teardownPolicyRouting(name)

	switch m.cfg.Backend {
	case config.BackendNetworkd:
		m.releaseViaNetworkd(ctx, name, mac)
	default:
		m.releaseViaDHClient(ctx, name)
	}
}

// ConfigureExisting recovers interfaces this app previously configured that lost
// their IP (e.g. after a daemon restart). Interfaces not tracked by a prior run
// are skipped so manually-configured or third-party interfaces are never touched.
func (m *Manager) ConfigureExisting(ctx context.Context) {
	if m.cfg.Backend == config.BackendNetworkd {
		m.migrateLegacyNetworkdFiles(ctx)
	}

	links, err := netlink.LinkList()
	if err != nil {
		m.logger.Error("failed to list interfaces", "error", err)
		return
	}

	for _, link := range links {
		name := link.Attrs().Name
		mac := link.Attrs().HardwareAddr
		if !m.ShouldManage(name) {
			continue
		}
		if m.cfg.Backend == config.BackendNetworkd {
			m.upgradeNameBasedFile(name, mac)
		}
		if !m.IsManagedByUs(name, mac) {
			m.logger.Debug("interface has no tracking file, skipping on startup", "interface", name)
			continue
		}
		if m.HasIP(name) {
			// Interface already has IP — restore policy routing in case daemon restarted.
			m.logger.Info("restoring policy routing for managed interface", "interface", name)
			go m.waitAndSetupPolicyRouting(ctx, name)
			continue
		}
		m.logger.Info("found previously-managed unconfigured interface at startup", "interface", name)
		if m.cfg.Backend == config.BackendNetworkd {
			// The .network file exists (so ConfigureInterface would skip it) but
			// the interface has no IP — ask networkd to (re)apply the config.
			go m.reconfigureViaNetworkd(ctx, name)
			continue
		}
		go m.ConfigureInterface(ctx, name)
	}
}

// migrateLegacyNetworkdFiles moves .network files written by older versions to
// the tmpfs directory /run/systemd/network into the persistent NetworkdDir, so
// that hot-plugged interfaces keep their IP across reboots.
func (m *Manager) migrateLegacyNetworkdFiles(ctx context.Context) {
	if strings.HasPrefix(filepath.Clean(m.cfg.NetworkdDir)+"/", "/run/") {
		m.logger.Warn("networkd_dir is on tmpfs; interface configuration will be lost on reboot",
			"networkd_dir", m.cfg.NetworkdDir, "recommended", config.DefaultNetworkdDir)
		return
	}

	matches, err := filepath.Glob(filepath.Join(config.LegacyNetworkdDir, "99-openstack-net-attach-*.network"))
	if err != nil || len(matches) == 0 {
		return
	}

	// systemd-networkd directories are conventionally world-readable (0755).
	if err := os.MkdirAll(m.cfg.NetworkdDir, 0755); err != nil { // #nosec G301 -- standard systemd network dir permissions
		m.logger.Warn("cannot create networkd dir for migration", "dir", m.cfg.NetworkdDir, "error", err)
		return
	}

	// os.Root confines all file access to these two directories, so a crafted
	// file name can never escape them (no path traversal, no symlink escape).
	srcRoot, err := os.OpenRoot(config.LegacyNetworkdDir)
	if err != nil {
		m.logger.Warn("cannot open legacy networkd dir", "dir", config.LegacyNetworkdDir, "error", err)
		return
	}
	defer srcRoot.Close()

	dstRoot, err := os.OpenRoot(m.cfg.NetworkdDir)
	if err != nil {
		m.logger.Warn("cannot open networkd dir", "dir", m.cfg.NetworkdDir, "error", err)
		return
	}
	defer dstRoot.Close()

	migrated := 0
	for _, match := range matches {
		file := filepath.Base(match)
		data, err := srcRoot.ReadFile(file)
		if err != nil {
			m.logger.Warn("failed to read legacy networkd file", "file", match, "error", err)
			continue
		}
		// systemd-networkd (user systemd-network) must be able to read the file.
		if err := dstRoot.WriteFile(file, data, 0644); err != nil {
			m.logger.Warn("failed to write networkd file", "file", file, "dir", m.cfg.NetworkdDir, "error", err)
			continue
		}
		_ = srcRoot.Remove(file)
		migrated++
		m.logger.Info("migrated networkd file to persistent dir", "from", match, "to", filepath.Join(m.cfg.NetworkdDir, file))
	}

	if migrated > 0 {
		m.networkctl(ctx, "reload")
	}
}

// reconfigureViaNetworkd brings the interface up and asks networkd to re-apply
// its .network file. Used at startup for managed interfaces that have no IP.
func (m *Manager) reconfigureViaNetworkd(ctx context.Context, name string) {
	if err := m.bringUp(name); err != nil {
		m.logger.Warn("failed to set interface up", "interface", name, "error", err)
	}
	m.networkctl(ctx, "reload")
	m.networkctl(ctx, "reconfigure", name)
	if err := m.waitAndSetupPolicyRouting(ctx, name); err != nil {
		m.logger.Warn("interface did not get an IP after reconfigure", "interface", name, "error", err)
	}
}

func (m *Manager) networkctl(ctx context.Context, args ...string) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Absolute binary path; args are fixed verbs plus a validated interface name
	// (see ValidInterfaceName), passed as argv without a shell.
	if out, err := exec.CommandContext(cctx, m.networkctlPath, args...).CombinedOutput(); err != nil { // #nosec G204 -- validated args, no shell
		m.logger.Warn("networkctl failed", "args", args, "error", err, "output", strings.TrimSpace(string(out)))
	}
}

// --- systemd-networkd backend ---

// networkdConfig renders the .network file for a NIC. It matches on the MAC
// address, not the interface name: after an OpenStack hard reboot / stop-start
// the hypervisor re-creates the VM and hot-plugged NICs can land in a different
// PCI slot, so ens7 comes back as ens4. The MAC address never changes.
// Type=ether keeps VLAN/bond/bridge devices that inherit the MAC from matching.
func networkdConfig(mac net.HardwareAddr) string {
	return fmt.Sprintf(`[Match]
MACAddress=%s
Type=ether

[Network]
DHCP=ipv4

[DHCP]
RouteMetric=100
UseDNS=true
UseDomains=true
UseMTU=true
`, mac.String())
}

func (m *Manager) configureViaNetworkd(ctx context.Context, name string, mac net.HardwareAddr) error {
	if len(mac) != 6 {
		return fmt.Errorf("interface %s has no Ethernet MAC address (%q)", name, mac.String())
	}

	if err := os.MkdirAll(m.cfg.NetworkdDir, 0755); err != nil { // #nosec G301 -- standard systemd network dir permissions
		return fmt.Errorf("creating networkd dir: %w", err)
	}

	networkFile := m.networkdFile(mac)
	if err := writeFileDurable(m.cfg.NetworkdDir, filepath.Base(networkFile), []byte(networkdConfig(mac))); err != nil {
		return fmt.Errorf("writing network file: %w", err)
	}

	reloadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(reloadCtx, m.networkctlPath, "reload") // #nosec G204 -- absolute path from trustedBinDirs, fixed args
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(networkFile)
		return fmt.Errorf("networkctl reload: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

func (m *Manager) releaseViaNetworkd(ctx context.Context, name string, mac net.HardwareAddr) {
	files := []string{m.nameBasedNetworkdFile(name)}
	if len(mac) > 0 {
		files = append(files, m.networkdFile(mac))
	}
	for _, networkFile := range files {
		if err := os.Remove(networkFile); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove networkd file", "file", networkFile, "error", err)
		}
	}

	reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(reloadCtx, m.networkctlPath, "reload") // #nosec G204 -- absolute path from trustedBinDirs, fixed args
	if output, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("networkctl reload failed on release", "error", err, "output", string(output))
	}
}

// isNetworkdManaged reports whether we wrote a .network file for this NIC,
// either the current MAC-keyed file or a name-keyed one from <= v1.1.5.
func (m *Manager) isNetworkdManaged(name string, mac net.HardwareAddr) bool {
	if len(mac) > 0 {
		if _, err := os.Stat(m.networkdFile(mac)); err == nil {
			return true
		}
	}
	_, err := os.Stat(m.nameBasedNetworkdFile(name))
	return err == nil
}

// networkdFile is the MAC-keyed .network file for a NIC, e.g.
// 99-openstack-net-attach-fa163ebac220.network.
func (m *Manager) networkdFile(mac net.HardwareAddr) string {
	key := strings.ReplaceAll(strings.ToLower(mac.String()), ":", "")
	return filepath.Join(m.cfg.NetworkdDir, fmt.Sprintf("99-openstack-net-attach-%s.network", key))
}

// nameBasedNetworkdFile is the file name used by versions <= 1.1.5, which
// matched on the interface name and broke when the name changed on reboot.
func (m *Manager) nameBasedNetworkdFile(name string) string {
	return filepath.Join(m.cfg.NetworkdDir, fmt.Sprintf("99-openstack-net-attach-%s.network", name))
}

// upgradeNameBasedFile replaces a name-matched .network file written by an
// older version with a MAC-matched one, while the name still points at the
// right NIC, so the configuration survives future renames.
func (m *Manager) upgradeNameBasedFile(name string, mac net.HardwareAddr) {
	if len(mac) != 6 {
		return
	}
	oldFile := m.nameBasedNetworkdFile(name)
	if _, err := os.Stat(oldFile); err != nil {
		return
	}
	newFile := m.networkdFile(mac)
	if err := writeFileDurable(m.cfg.NetworkdDir, filepath.Base(newFile), []byte(networkdConfig(mac))); err != nil {
		m.logger.Warn("failed to upgrade networkd file to MAC match", "interface", name, "error", err)
		return
	}
	if err := os.Remove(oldFile); err != nil {
		m.logger.Warn("failed to remove name-based networkd file", "file", oldFile, "error", err)
	}
	m.logger.Info("upgraded networkd file to MAC-based match", "interface", name, "from", oldFile, "to", newFile)
}

// writeFileDurable writes dir/file atomically and fsyncs it, so the file is
// complete on disk even if the VM is powered off right after (an OpenStack hard
// reboot is a power cut from the guest's point of view). 0644 is required:
// systemd-networkd runs as user systemd-network and must read the file.
func writeFileDurable(dir, file string, data []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	tmp := "." + file + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, file); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	// Persist the directory entry too.
	if d, err := root.Open("."); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// waitUdevInitialized waits (max 5s) until udev has finished processing the
// interface, i.e. renamed it and written /run/udev/data/n<ifindex>. This is the
// same "initialized" signal systemd-networkd waits for.
func (m *Manager) waitUdevInitialized(ctx context.Context, index int) {
	dbFile := fmt.Sprintf("/run/udev/data/n%d", index)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dbFile); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	m.logger.Debug("udev did not report interface as initialized, continuing", "ifindex", index)
}

// --- dhclient backend ---

func (m *Manager) configureViaDHClient(ctx context.Context, name string) error {
	if m.isDHClientRunning(name) {
		m.logger.Debug("dhclient already running for interface", "interface", name)
		return nil
	}

	if err := os.MkdirAll("/var/lib/dhcp", 0755); err != nil { // #nosec G301 -- standard Debian/Ubuntu dhcp state dir permissions
		return fmt.Errorf("creating dhcp dir: %w", err)
	}

	dhctx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.DHCPTimeout)*time.Second)
	defer cancel()

	// dhclient -1: exit on initial failure (don't retry forever on first attempt).
	// After getting a lease, dhclient forks to background and handles renewals.
	// Absolute binary path, validated interface name, argv without a shell.
	cmd := exec.CommandContext(dhctx, m.dhclientPath, // #nosec G204 -- validated args, no shell
		"-1",
		"-v",
		"-pf", m.dhclientPIDFile(name),
		"-lf", m.dhclientLeaseFile(name),
		name,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dhclient: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

func (m *Manager) releaseViaDHClient(ctx context.Context, name string) {
	pidFile := m.dhclientPIDFile(name)

	releaseCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(releaseCtx, m.dhclientPath, // #nosec G204 -- validated args, no shell
		"-r",
		"-pf", pidFile,
		"-lf", m.dhclientLeaseFile(name),
		name,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		m.logger.Debug("dhclient release result", "output", string(output), "error", err)
	}

	_ = os.Remove(pidFile)
}

func (m *Manager) isDHClientRunning(name string) bool {
	data, err := os.ReadFile(m.dhclientPIDFile(name))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func (m *Manager) dhclientPIDFile(iface string) string {
	return fmt.Sprintf("/run/openstack-net-attach-dhclient-%s.pid", iface)
}

func (m *Manager) dhclientLeaseFile(iface string) string {
	return fmt.Sprintf("/var/lib/dhcp/openstack-net-attach-%s.leases", iface)
}

// --- helpers ---

func (m *Manager) bringUp(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("link not found: %w", err)
	}
	return netlink.LinkSetUp(link)
}

func (m *Manager) waitForCarrier(ctx context.Context, name string) error {
	deadline := time.Now().Add(time.Duration(m.cfg.CarrierTimeout) * time.Second)

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		link, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("interface disappeared: %w", err)
		}

		attrs := link.Attrs()
		rawFlags := attrs.RawFlags
		isUp := rawFlags&uint32(syscall.IFF_UP) != 0
		isRunning := rawFlags&uint32(syscall.IFF_RUNNING) != 0

		if isUp && isRunning {
			return nil
		}

		// virtio-net on OpenStack may report OperUnknown even when working
		if isUp && (attrs.OperState == netlink.OperUp || attrs.OperState == netlink.OperUnknown) {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("carrier timeout after %ds for %s", m.cfg.CarrierTimeout, name)
}

// waitAndSetupPolicyRouting waits until the interface has an IP (up to 30s)
// then calls setupPolicyRouting. Used for networkd where IP is assigned async.
func (m *Manager) waitAndSetupPolicyRouting(ctx context.Context, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.HasIP(name) {
			return m.setupPolicyRouting(name)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for IP on %s for policy routing", name)
}

// --- policy routing ---

// policyTableID returns a stable routing table ID (200–399) derived from the
// interface name so that each interface gets its own table across restarts.
func policyTableID(name string) int {
	var h uint32 = 2166136261 // FNV-1a basis
	for _, b := range []byte(name) {
		h ^= uint32(b)
		h *= 16777619
	}
	return int(200 + h%200)
}

// setupPolicyRouting adds a per-source routing rule so that packets sent FROM
// this interface's IP always leave via the same interface. This prevents the
// asymmetric-routing problem where nginx (or any service) replies go out the
// wrong gateway when multiple default routes exist.
func (m *Manager) setupPolicyRouting(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("link lookup: %w", err)
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("addr list: %w", err)
	}

	var srcIP net.IP
	for _, addr := range addrs {
		if addr.IP != nil && !addr.IP.IsLinkLocalUnicast() {
			srcIP = addr.IP.To4()
			break
		}
	}
	if srcIP == nil {
		return fmt.Errorf("no IPv4 address on %s", name)
	}

	tableID := policyTableID(name)

	// Copy only the connected subnet route and the default route into the policy
	// table. These two are sufficient: subnet route handles local traffic, default
	// handles replies to clients outside the subnet. Other DHCP-injected host
	// routes (8.8.8.8, 169.254.169.254, etc.) are not needed here.
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("route list: %w", err)
	}
	for _, r := range routes {
		isDefault := r.Dst == nil || r.Dst.String() == "0.0.0.0/0"
		isConnected := r.Dst != nil && r.Gw == nil
		if !isDefault && !isConnected {
			continue
		}
		nr := r
		nr.Table = tableID
		if addErr := netlink.RouteAdd(&nr); addErr != nil && addErr != syscall.EEXIST {
			m.logger.Debug("policy route add skipped", "dst", nr.Dst, "error", addErr)
		}
		// Remove the default route from the main table so only one default route
		// exists (via the primary/management interface). Policy routing handles
		// outgoing traffic from this interface's IP via the policy table.
		if isDefault {
			if delErr := netlink.RouteDel(&r); delErr != nil {
				m.logger.Debug("main table default route already removed", "gw", r.Gw, "error", delErr)
			} else {
				m.logger.Info("removed default route from main table", "interface", name, "gw", r.Gw)
			}
		}
	}

	// Add rule: from <IP>/32 lookup tableID priority 100
	rule := netlink.NewRule()
	rule.Src = &net.IPNet{IP: srcIP, Mask: net.CIDRMask(32, 32)}
	rule.Table = tableID
	rule.Priority = 100
	if err := netlink.RuleAdd(rule); err != nil && err != syscall.EEXIST {
		return fmt.Errorf("rule add: %w", err)
	}

	m.logger.Info("policy routing configured", "interface", name, "src", srcIP, "table", tableID)
	return nil
}

// teardownPolicyRouting removes the ip rules added by setupPolicyRouting.
// The kernel cleans up routes when the interface goes away, but ip rules persist
// and must be removed manually.
func (m *Manager) teardownPolicyRouting(name string) {
	tableID := policyTableID(name)
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		m.logger.Debug("policy rule list failed", "error", err)
		return
	}
	for _, r := range rules {
		if r.Table == tableID {
			if err := netlink.RuleDel(&r); err != nil {
				m.logger.Debug("policy rule del failed", "error", err)
			}
		}
	}
}
