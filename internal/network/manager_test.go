package network

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yesveer/openstack-net-attach/internal/config"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.NetworkdDir = t.TempDir()
	return NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestNetworkdConfigMatchesMAC(t *testing.T) {
	mac, _ := net.ParseMAC("fa:16:3e:ba:c2:20")
	got := networkdConfig(mac)
	for _, want := range []string{"MACAddress=fa:16:3e:ba:c2:20", "Type=ether", "DHCP=ipv4"} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Name=") {
		t.Errorf("config must not match on interface name:\n%s", got)
	}
}

func TestNetworkdFileIsKeyedByMAC(t *testing.T) {
	m := testManager(t)
	mac, _ := net.ParseMAC("FA:16:3E:BA:C2:20")
	if got := filepath.Base(m.networkdFile(mac)); got != "99-openstack-net-attach-fa163ebac220.network" {
		t.Fatalf("unexpected file name %q", got)
	}
}

func TestManagedSurvivesRename(t *testing.T) {
	m := testManager(t)
	mac, _ := net.ParseMAC("fa:16:3e:ba:c2:20")
	if err := writeFileDurable(m.cfg.NetworkdDir, filepath.Base(m.networkdFile(mac)), []byte(networkdConfig(mac))); err != nil {
		t.Fatal(err)
	}
	// Configured as ens7, comes back as ens4 after a hard reboot.
	if !m.isNetworkdManaged("ens4", mac) {
		t.Fatal("NIC should still be recognised as managed after rename")
	}
	other, _ := net.ParseMAC("fa:16:3e:00:00:01")
	if m.isNetworkdManaged("ens5", other) {
		t.Fatal("unrelated NIC must not be treated as managed")
	}
}

func TestUpgradeNameBasedFile(t *testing.T) {
	m := testManager(t)
	mac, _ := net.ParseMAC("fa:16:3e:ba:c2:20")
	old := m.nameBasedNetworkdFile("ens7")
	if err := os.WriteFile(old, []byte("[Match]\nName=ens7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.upgradeNameBasedFile("ens7", mac)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old name-based file should be removed, stat err=%v", err)
	}
	data, err := os.ReadFile(m.networkdFile(mac))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "MACAddress=fa:16:3e:ba:c2:20") {
		t.Errorf("new file does not match on MAC:\n%s", data)
	}
	st, _ := os.Stat(m.networkdFile(mac))
	if st.Mode().Perm()&0o044 != 0o044 {
		t.Errorf("file must be readable by systemd-networkd, got %v", st.Mode().Perm())
	}
}

func TestValidInterfaceName(t *testing.T) {
	for name, want := range map[string]bool{
		"ens4": true, "eth0": true, "enp0s4": true, "ens4.100": true,
		"": false, "..": false, "a/b": false, "ens4\nName=*": false, "verylonginterfacename": false,
	} {
		if got := ValidInterfaceName(name); got != want {
			t.Errorf("ValidInterfaceName(%q) = %v, want %v", name, got, want)
		}
	}
}
