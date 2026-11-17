package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Backend selects how DHCP is configured for hot-plugged interfaces.
// "networkd" (default) uses systemd-networkd .network files (recommended for Ubuntu 22.04+).
// "dhclient" invokes dhclient directly.
type Backend string

// DefaultNetworkdDir is persistent across reboots.
const DefaultNetworkdDir = "/etc/systemd/network"

// LegacyNetworkdDir was used by versions <= 1.1.3. It lives on tmpfs, so files
// written there are lost on reboot (interface stays, IP disappears).
const LegacyNetworkdDir = "/run/systemd/network"

const (
	BackendNetworkd Backend = "networkd"
	BackendDHClient Backend = "dhclient"
)

type Config struct {
	// Backend to use for DHCP configuration
	Backend Backend `yaml:"backend"`

	// Network interface name prefixes to manage
	IncludePrefixes []string `yaml:"include_prefixes"`

	// Network interface name prefixes to ignore
	ExcludePrefixes []string `yaml:"exclude_prefixes"`

	// DHCPTimeout is the maximum seconds to wait for a DHCP lease (dhclient backend)
	DHCPTimeout int `yaml:"dhcp_timeout"`

	// MaxRetries is the number of DHCP attempts before giving up
	MaxRetries int `yaml:"max_retries"`

	// RetryDelay is the seconds to wait between DHCP retry attempts
	RetryDelay int `yaml:"retry_delay"`

	// CarrierTimeout is the maximum seconds to wait for link carrier before starting DHCP
	CarrierTimeout int `yaml:"carrier_timeout"`

	// NetworkdDir is the directory where systemd-networkd .network files are written.
	// Must be a persistent directory (e.g. /etc/systemd/network) so that the
	// configuration survives a reboot; /run is tmpfs and is wiped on boot.
	NetworkdDir string `yaml:"networkd_dir"`
}

func Default() *Config {
	return &Config{
		Backend:         BackendNetworkd,
		IncludePrefixes: []string{"eth", "ens", "enp", "eno", "em"},
		ExcludePrefixes: []string{"lo", "docker", "br-", "veth", "cali", "virbr", "tun", "tap", "dummy", "flannel", "weave"},
		DHCPTimeout:     60,
		MaxRetries:      5,
		RetryDelay:      5,
		CarrierTimeout:  30,
		NetworkdDir:     DefaultNetworkdDir,
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Backend != BackendNetworkd && cfg.Backend != BackendDHClient {
		return nil, fmt.Errorf("invalid backend %q: must be 'networkd' or 'dhclient'", cfg.Backend)
	}

	return cfg, nil
}
