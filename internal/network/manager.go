package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/yesveer/openstack-network-manager/internal/config"
)

// Manager handles network interface configuration for hot-plugged OpenStack NICs.
type Manager struct {
	cfg         *config.Config
	logger      *slog.Logger
	configuring sync.Map // tracks interfaces currently being configured
}

func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	return &Manager{
		cfg:    cfg,
		logger: logger,
	}
}

// ShouldManage returns true if the daemon should manage this interface.
func (m *Manager) ShouldManage(name string) bool {
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
	// Only one goroutine may configure a given interface at a time.
	if _, loaded := m.configuring.LoadOrStore(name, struct{}{}); loaded {
		m.logger.Debug("interface configuration already in progress", "interface", name)
		return
	}
	defer m.configuring.Delete(name)

	log := m.logger.With("interface", name)

	if m.HasIP(name) {
		log.Debug("interface already has an IP address, skipping")
		return
	}

	if m.isNetworkdManaged(name) {
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
			lastErr = m.configureViaNetworkd(ctx, name)
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
func (m *Manager) IsManagedByUs(name string) bool {
	switch m.cfg.Backend {
	case config.BackendNetworkd:
		return m.isNetworkdManaged(name)
	default:
		_, err := os.Stat(m.dhclientLeaseFile(name))
		return err == nil
	}
}

// ReleaseInterface releases the DHCP lease and removes configuration for an interface.
// It only acts on interfaces that were configured by this app; others are ignored.
func (m *Manager) ReleaseInterface(ctx context.Context, name string) {
	log := m.logger.With("interface", name)

	if !m.IsManagedByUs(name) {
		log.Debug("interface was not configured by us, skipping release")
		return
	}

	log.Info("interface removed, releasing DHCP")

	m.teardownPolicyRouting(name)

	switch m.cfg.Backend {
	case config.BackendNetworkd:
		m.releaseViaNetworkd(ctx, name)
	default:
		m.releaseViaDHClient(ctx, name)
	}
}

// ConfigureExisting recovers interfaces this app previously configured that lost
// their IP (e.g. after a daemon restart). Interfaces not tracked by a prior run
// are skipped so manually-configured or third-party interfaces are never touched.
func (m *Manager) ConfigureExisting(ctx context.Context) {
	links, err := netlink.LinkList()
	if err != nil {
		m.logger.Error("failed to list interfaces", "error", err)
		return
	}

	for _, link := range links {
		name := link.Attrs().Name
		if !m.ShouldManage(name) {
			continue
		}
		if !m.IsManagedByUs(name) {
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
		go m.ConfigureInterface(ctx, name)
	}
}

// --- systemd-networkd backend ---

func (m *Manager) configureViaNetworkd(ctx context.Context, name string) error {
	content := fmt.Sprintf(`[Match]
Name=%s

[Network]
DHCP=ipv4

[DHCP]
RouteMetric=100
UseDNS=true
UseDomains=true
UseMTU=true
`, name)

	if err := os.MkdirAll(m.cfg.NetworkdDir, 0755); err != nil {
		return fmt.Errorf("creating networkd dir: %w", err)
	}

	networkFile := m.networkdFile(name)
	if err := os.WriteFile(networkFile, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing network file: %w", err)
	}

	reloadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(reloadCtx, "networkctl", "reload")
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(networkFile)
		return fmt.Errorf("networkctl reload: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

func (m *Manager) releaseViaNetworkd(ctx context.Context, name string) {
	networkFile := m.networkdFile(name)
	if err := os.Remove(networkFile); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove networkd file", "file", networkFile, "error", err)
	}

	reloadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(reloadCtx, "networkctl", "reload")
	if output, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("networkctl reload failed on release", "error", err, "output", string(output))
	}
}

func (m *Manager) isNetworkdManaged(name string) bool {
	_, err := os.Stat(m.networkdFile(name))
	return err == nil
}

func (m *Manager) networkdFile(name string) string {
	return fmt.Sprintf("%s/99-openstack-network-manager-%s.network", m.cfg.NetworkdDir, name)
}

// --- dhclient backend ---

func (m *Manager) configureViaDHClient(ctx context.Context, name string) error {
	if m.isDHClientRunning(name) {
		m.logger.Debug("dhclient already running for interface", "interface", name)
		return nil
	}

	if err := os.MkdirAll("/var/lib/dhcp", 0755); err != nil {
		return fmt.Errorf("creating dhcp dir: %w", err)
	}

	dhctx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.DHCPTimeout)*time.Second)
	defer cancel()

	// dhclient -1: exit on initial failure (don't retry forever on first attempt).
	// After getting a lease, dhclient forks to background and handles renewals.
	cmd := exec.CommandContext(dhctx, "dhclient",
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

	cmd := exec.CommandContext(releaseCtx, "dhclient",
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
	return fmt.Sprintf("/run/openstack-network-manager-dhclient-%s.pid", iface)
}

func (m *Manager) dhclientLeaseFile(iface string) string {
	return fmt.Sprintf("/var/lib/dhcp/openstack-network-manager-%s.leases", iface)
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
