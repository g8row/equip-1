package network

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/godbus/dbus/v5"
)

const (
	connmanName = "net.connman"
	pathManager = "/"
	pathAgent   = "/com/equip1/companion/agent"

	ifaceManager    = "net.connman.Manager"
	ifaceTechnology = "net.connman.Technology"
	ifaceService    = "net.connman.Service"
	ifaceAgent      = "net.connman.Agent"
)

// NetworkStatus holds the current state of ConnMan networking.
type NetworkStatus struct {
	State    string // "idle", "association", "configuration", "ready", "online", "disconnect"
	SSID     string
	IPv4     string
	AP       bool
	ErrorMsg string
}

// Manager wraps the D-Bus connection to ConnMan.
type Manager struct {
	conn *dbus.Conn
}

// NewManager connects to the system D-Bus and returns a ConnMan manager.
func NewManager() (*Manager, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("system bus: %w", err)
	}
	return &Manager{conn: conn}, nil
}

// Close closes the D-Bus connection.
func (m *Manager) Close() {
	m.conn.Close()
}

// GetStatus retrieves the current WiFi and AP status.
func (m *Manager) GetStatus() (NetworkStatus, error) {
	var status NetworkStatus

	// 1. Check WiFi Technology for Tethering (AP) state
	techProps, err := m.getTechnologyProperties("wifi")
	if err != nil {
		return status, fmt.Errorf("wifi technology: %w", err)
	}
	if tethering, ok := techProps["Tethering"].Value().(bool); ok && tethering {
		status.AP = true
		status.State = "ready"
		status.SSID, _ = techProps["TetheringIdentifier"].Value().(string)
		return status, nil
	}

	// 2. Check Services for active connection
	services, err := m.getServices()
	if err != nil {
		return status, fmt.Errorf("services: %w", err)
	}

	activeStates := map[string]bool{"ready": true, "online": true, "association": true, "configuration": true}
	for _, srv := range services {
		props := srv.Props
		typ, _ := props["Type"].Value().(string)
		state, _ := props["State"].Value().(string)
		if !activeStates[state] {
			continue
		}
		if ipv4, ok := props["IPv4"].Value().(map[string]dbus.Variant); ok {
			if addr, ok := ipv4["Address"].Value().(string); ok {
				status.IPv4 = addr
			}
		}
		status.State = state
		switch typ {
		case "wifi":
			status.SSID, _ = props["Name"].Value().(string)
			return status, nil
		case "ethernet":
			// Ethernet is always preferred; keep scanning for WiFi too but
			// return ethernet if no WiFi service is active.
			status.SSID = ""
		}
	}

	if status.State != "" {
		return status, nil
	}
	status.State = "idle"
	return status, nil
}

// SetAP enables or disables the ConnMan WiFi AP (tethering).
func (m *Manager) SetAP(enable bool, ssid, passphrase string) error {
	obj := m.conn.Object(connmanName, "/net/connman/technology/wifi")
	
	if enable {
		if ssid != "" {
			if err := obj.Call(ifaceTechnology+".SetProperty", 0, "TetheringIdentifier", dbus.MakeVariant(ssid)).Err; err != nil {
				return fmt.Errorf("set AP ssid: %w", err)
			}
		}
		if passphrase != "" {
			if err := obj.Call(ifaceTechnology+".SetProperty", 0, "TetheringPassphrase", dbus.MakeVariant(passphrase)).Err; err != nil {
				return fmt.Errorf("set AP passphrase: %w", err)
			}
		}
	}
	
	if err := obj.Call(ifaceTechnology+".SetProperty", 0, "Tethering", dbus.MakeVariant(enable)).Err; err != nil {
		return fmt.Errorf("set AP tethering=%v: %w", enable, err)
	}
	return nil
}

// Connect attempts to connect to a WiFi network.
func (m *Manager) Connect(ctx context.Context, ssid, passphrase string) error {
	// First, disable AP if it's on
	_ = m.SetAP(false, "", "")

	// Start agent to handle passphrase if needed
	agent := newAgent(passphrase)
	if err := m.conn.Export(agent, pathAgent, ifaceAgent); err != nil {
		return fmt.Errorf("export agent: %w", err)
	}
	defer m.conn.Export(nil, pathAgent, ifaceAgent)

	mgrObj := m.conn.Object(connmanName, pathManager)
	if err := mgrObj.Call(ifaceManager+".RegisterAgent", 0, dbus.ObjectPath(pathAgent)).Err; err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	defer mgrObj.Call(ifaceManager+".UnregisterAgent", 0, dbus.ObjectPath(pathAgent))

	// Find the service path for the given SSID
	// This might require scanning if the service is not currently known
	path, err := m.findWifiService(ssid)
	if err != nil {
		// As a fallback for unlisted networks, ConnMan supports ConnectProvider or Hidden networks,
		// but typically we can trigger a scan or just match existing known/scanned ones.
		return fmt.Errorf("find wifi service %q: %w", ssid, err)
	}

	srvObj := m.conn.Object(connmanName, path)
	call := srvObj.CallWithContext(ctx, ifaceService+".Connect", 0)
	if call.Err != nil {
		return fmt.Errorf("connect %q: %w", ssid, call.Err)
	}

	return nil
}

// WifiNetwork describes a network visible to ConnMan.
type WifiNetwork struct {
	SSID     string `json:"ssid"`
	Strength int    `json:"strength"`
	State    string `json:"state"`
}

// ScanWifi triggers a ConnMan WiFi scan and returns the resulting services.
func (m *Manager) ScanWifi(ctx context.Context) ([]WifiNetwork, error) {
	tech := m.conn.Object(connmanName, "/net/connman/technology/wifi")
	if err := tech.CallWithContext(ctx, ifaceTechnology+".Scan", 0).Err; err != nil {
		slog.Warn("connman scan trigger failed (continuing)", "error", err)
	}
	services, err := m.getServices()
	if err != nil {
		return nil, err
	}
	var out []WifiNetwork
	seen := map[string]bool{}
	for _, srv := range services {
		if typ, _ := srv.Props["Type"].Value().(string); typ != "wifi" {
			continue
		}
		name, _ := srv.Props["Name"].Value().(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		strength := 0
		if s, ok := srv.Props["Strength"].Value().(uint8); ok {
			strength = int(s)
		}
		state, _ := srv.Props["State"].Value().(string)
		out = append(out, WifiNetwork{SSID: name, Strength: strength, State: state})
	}
	return out, nil
}

// Disconnect disconnects the current WiFi network.
func (m *Manager) Disconnect(ssid string) error {
	path, err := m.findWifiService(ssid)
	if err != nil {
		return err
	}
	srvObj := m.conn.Object(connmanName, path)
	if err := srvObj.Call(ifaceService+".Disconnect", 0).Err; err != nil {
		return fmt.Errorf("disconnect: %w", err)
	}
	return nil
}

// -- Helpers --

type serviceInfo struct {
	Path  dbus.ObjectPath
	Props map[string]dbus.Variant
}

func (m *Manager) getServices() ([]serviceInfo, error) {
	obj := m.conn.Object(connmanName, pathManager)
	var out []serviceInfo
	err := obj.Call(ifaceManager+".GetServices", 0).Store(&out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Manager) getTechnologyProperties(typ string) (map[string]dbus.Variant, error) {
	obj := m.conn.Object(connmanName, dbus.ObjectPath("/net/connman/technology/"+typ))
	var props map[string]dbus.Variant
	err := obj.Call(ifaceTechnology+".GetProperties", 0).Store(&props)
	if err != nil {
		return nil, err
	}
	return props, nil
}

func (m *Manager) findWifiService(ssid string) (dbus.ObjectPath, error) {
	services, err := m.getServices()
	if err != nil {
		return "", err
	}
	for _, srv := range services {
		typ, _ := srv.Props["Type"].Value().(string)
		name, _ := srv.Props["Name"].Value().(string)
		if typ == "wifi" && name == ssid {
			return srv.Path, nil
		}
	}
	return "", fmt.Errorf("network not found")
}

// -- Agent --

type agent struct {
	passphrase string
}

func newAgent(passphrase string) *agent {
	return &agent{passphrase: passphrase}
}

func (a *agent) Release() *dbus.Error {
	return nil
}

func (a *agent) ReportError(service dbus.ObjectPath, error string) *dbus.Error {
	slog.Error("connman agent error", "service", service, "error", error)
	return nil
}

func (a *agent) RequestBrowser(service dbus.ObjectPath, url string) *dbus.Error {
	return nil
}

func (a *agent) RequestInput(service dbus.ObjectPath, fields map[string]map[string]dbus.Variant) (map[string]dbus.Variant, *dbus.Error) {
	slog.Info("connman agent requested input", "service", service)
	out := make(map[string]dbus.Variant)
	if _, ok := fields["Passphrase"]; ok {
		out["Passphrase"] = dbus.MakeVariant(a.passphrase)
	}
	return out, nil
}

func (a *agent) Cancel() *dbus.Error {
	return nil
}
