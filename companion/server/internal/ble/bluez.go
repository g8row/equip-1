package ble

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"equip1/companion/server/internal/network"
)

const (
	bluezName = "org.bluez"

	ifaceObjectManager        = "org.freedesktop.DBus.ObjectManager"
	ifaceProperties           = "org.freedesktop.DBus.Properties"
	ifaceDBusDaemon           = "org.freedesktop.DBus"
	ifaceAdapter              = "org.bluez.Adapter1"
	ifaceGattManager          = "org.bluez.GattManager1"
	ifaceLEAdvertisingManager = "org.bluez.LEAdvertisingManager1"
	ifaceGattService          = "org.bluez.GattService1"
	ifaceGattCharacteristic   = "org.bluez.GattCharacteristic1"
	ifaceAdvertisement        = "org.bluez.LEAdvertisement1"

	memberInterfacesAdded   = ifaceObjectManager + ".InterfacesAdded"
	memberInterfacesRemoved = ifaceObjectManager + ".InterfacesRemoved"
	memberNameOwnerChanged  = ifaceDBusDaemon + ".NameOwnerChanged"

	serviceUUID       = "e2710000-b5a3-f393-e0a9-e50e24dcca9e"
	statusUUID        = "e2710001-b5a3-f393-e0a9-e50e24dcca9e"
	wifiCredsUUID     = "e2710002-b5a3-f393-e0a9-e50e24dcca9e"
	apControlUUID     = "e2710003-b5a3-f393-e0a9-e50e24dcca9e"
	recordControlUUID = "e2710004-b5a3-f393-e0a9-e50e24dcca9e"
	networkResultUUID = "e2710005-b5a3-f393-e0a9-e50e24dcca9e"
	wifiScanUUID      = "e2710006-b5a3-f393-e0a9-e50e24dcca9e"

	appPath     dbus.ObjectPath = "/com/equip1/companion"
	servicePath dbus.ObjectPath = "/com/equip1/companion/service0"
	adPath      dbus.ObjectPath = "/com/equip1/companion/advertisement0"

	// apPassphrase aliases the single source of truth in internal/network so
	// callers in this package don't need an extra import. See
	// network.APPassphrase's doc comment.
	apPassphrase = network.APPassphrase
)

// Provisioner is satisfied by provisioning.Manager; kept as an interface to
// avoid an import cycle.
type Provisioner interface {
	ApplyCredentials(ctx context.Context, ssid, passphrase string) error
}

// NetworkController is satisfied by network.Manager.
type NetworkController interface {
	SetAP(enable bool, ssid, passphrase string) error
	ScanWifi(ctx context.Context) ([]network.WifiNetwork, error)
}

// wifiScanCacheTTL controls how long cached scan results are served before a
// read triggers a fresh background scan. ConnMan scans take several seconds —
// far longer than a single characteristic read should block — so reads always
// return immediately from cache while a stale cache triggers a refresh.
const wifiScanCacheTTL = 20 * time.Second

// Server owns the BlueZ advertisement and local GATT application.
type Server struct {
	conn   *dbus.Conn
	api    *APIClient
	name   string
	prov   Provisioner
	netCtl NetworkController

	app    *application
	ad     *advertisement
	status *characteristic
	netres *characteristic
	ticker *time.Ticker
	done   chan struct{}

	// adapterMu guards the fields below, which change as the BT adapter
	// (a USB device) disappears and reappears — see watchAdapterEvents.
	adapterMu   sync.Mutex
	adapterPath dbus.ObjectPath
	hciAdv      *legacyAdvertiser
	started     bool // Start() has run at least once
	registered  bool // currently registered with a live adapter

	scanMu      sync.Mutex
	scanResults []byte
	scanAt      time.Time
	scanning    bool
}

// Options configures the BLE server.
type Options struct {
	APIBase string
	Name    string
	Prov    Provisioner      // optional; enables WiFi credential writes
	NetCtl  NetworkController // optional; enables AP control writes
}

// NewServer creates a BLE server. Start registers it with BlueZ.
func NewServer(opts Options) (*Server, error) {
	if opts.APIBase == "" {
		opts.APIBase = "http://127.0.0.1:8000"
	}
	if opts.Name == "" {
		opts.Name = "equip-1"
	}
	// Use a private bus connection, not the shared dbus.SystemBus() — that
	// connection is also held by network.Manager, and both call Close() on
	// their own lifecycle. A shared conn means server.Stop() (e.g. on BLE
	// construction retry) can pull the rug out from under ConnMan traffic.
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("connect system bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("authenticate system bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("system bus hello: %w", err)
	}
	s := &Server{
		conn:   conn,
		api:    NewAPIClient(opts.APIBase),
		name:   opts.Name,
		prov:   opts.Prov,
		netCtl: opts.NetCtl,
		done:   make(chan struct{}),
	}
	s.buildObjects()
	return s, nil
}

func (s *Server) buildObjects() {
	service := &gattService{path: servicePath}
	s.status = newCharacteristic(statusUUID, servicePath, "status", []string{"read", "notify"})
	// encrypt-write only, deliberately without plain "write": BlueZ ORs a
	// characteristic's permission flags together, so keeping "write" alongside
	// "encrypt-write" would let an unencrypted write request satisfy either
	// permission and nullify the encryption requirement.
	wifiCreds := newCharacteristic(wifiCredsUUID, servicePath, "wifi_creds", []string{"encrypt-write"})
	apControl := newCharacteristic(apControlUUID, servicePath, "ap_control", []string{"encrypt-write"})
	recordControl := newCharacteristic(recordControlUUID, servicePath, "record_control", []string{"encrypt-write"})
	s.netres = newCharacteristic(networkResultUUID, servicePath, "network_result", []string{"read", "notify"})
	wifiScan := newCharacteristic(wifiScanUUID, servicePath, "wifi_scan", []string{"read"})

	s.status.read = func(ctx context.Context) ([]byte, *dbus.Error) {
		return s.api.StatusPayload(ctx), nil
	}
	recordControl.write = func(ctx context.Context, value []byte) *dbus.Error {
		if len(value) == 0 {
			return dbus.MakeFailedError(fmt.Errorf("missing record command"))
		}
		if err := s.api.Record(ctx, value[0]); err != nil {
			return dbus.MakeFailedError(err)
		}
		return nil
	}
	wifiCreds.write = func(ctx context.Context, value []byte) *dbus.Error {
		if s.prov == nil {
			return s.networkResult("error", "provisioner not available")
		}
		var creds struct {
			SSID string `json:"ssid"`
			PSK  string `json:"psk"`
		}
		if err := json.Unmarshal(value, &creds); err != nil || creds.SSID == "" {
			return s.networkResult("error", "invalid wifi credentials")
		}
		go func() {
			applyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.prov.ApplyCredentials(applyCtx, creds.SSID, creds.PSK); err != nil {
				slog.Warn("ble-wifi-creds-apply-failed", "ssid", creds.SSID, "error", err)
				s.networkResult("error", err.Error())
				return
			}
			slog.Info("ble-wifi-creds-applied", "ssid", creds.SSID)
			s.networkResult("connected", creds.SSID)
		}()
		// Immediately emit "connecting" so the client knows we received the creds.
		s.networkResult("connecting", creds.SSID)
		return nil
	}
	apControl.write = func(ctx context.Context, value []byte) *dbus.Error {
		if s.netCtl == nil {
			return s.networkResult("error", "network controller not available")
		}
		if len(value) == 0 {
			return dbus.MakeFailedError(fmt.Errorf("missing AP command"))
		}
		enable := value[0] != 0
		if err := s.netCtl.SetAP(enable, s.name, apPassphrase); err != nil {
			return s.networkResult("error", err.Error())
		}
		if enable {
			s.networkResult("ap", s.name)
		} else {
			s.networkResult("idle", "")
		}
		return nil
	}
	s.netres.read = func(ctx context.Context) ([]byte, *dbus.Error) {
		return s.netres.Value(), nil
	}
	wifiScan.read = func(ctx context.Context) ([]byte, *dbus.Error) {
		return s.wifiScanPayload(), nil
	}

	objects := []managedObject{
		service,
		s.status,
		wifiCreds,
		apControl,
		recordControl,
		s.netres,
		wifiScan,
	}
	s.app = &application{objects: objects}
	s.ad = &advertisement{
		path:         adPath,
		localName:    shortName(s.name),
		serviceUUIDs: []string{serviceUUID},
	}
	s.netres.setValue([]byte(`{"state":"idle","ssid":null,"ip":null,"err":null}`))
}

// Start exports the GATT objects, watches BlueZ for adapter lifecycle events,
// then powers the adapter and registers with BlueZ. It blocks (respecting
// ctx) until an adapter is available — the AIC8800 is USB and often has not
// enumerated by the time systemd's After=bluetooth.service is satisfied, so
// waiting here (instead of failing) fixes the boot-time crash loop.
func (s *Server) Start(ctx context.Context) error {
	if s.isStarted() {
		return nil
	}

	// Exporting is independent of any adapter and idempotent (re-Export just
	// replaces the handler for a path/interface), so it can run unconditionally.
	if err := s.exportObjects(); err != nil {
		return err
	}

	// Watch BlueZ before searching for the adapter, so no InterfacesAdded
	// between the search and the watch going live can be missed.
	sigCh := make(chan *dbus.Signal, 32)
	s.conn.Signal(sigCh)
	if err := s.conn.AddMatchSignal(
		dbus.WithMatchSender(bluezName),
		dbus.WithMatchInterface(ifaceObjectManager),
	); err != nil {
		return fmt.Errorf("watch bluez object manager: %w", err)
	}
	if err := s.conn.AddMatchSignal(
		dbus.WithMatchInterface(ifaceDBusDaemon),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, bluezName),
	); err != nil {
		return fmt.Errorf("watch bluez name owner: %w", err)
	}

	adapterPath, err := s.findAdapter()
	if err != nil {
		slog.Warn("ble-adapter-not-found-waiting", "error", err)
		if adapterPath, err = s.waitForAdapter(ctx, sigCh); err != nil {
			return err
		}
	}

	if err := s.registerWithAdapter(adapterPath); err != nil {
		return err
	}

	s.setStarted(true)
	s.ticker = time.NewTicker(5 * time.Second)
	go s.notifyLoop(ctx)
	go s.watchAdapterEvents(ctx, sigCh)
	slog.Info("ble-started", "adapter", adapterPath, "name", s.name, "service_uuid", serviceUUID)
	return nil
}

// waitForAdapter blocks until a BlueZ adapter with GATT + LE advertising
// support shows up, driven by the ObjectManager watch set up in Start rather
// than polling.
func (s *Server) waitForAdapter(ctx context.Context, sigCh chan *dbus.Signal) (dbus.ObjectPath, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case sig, ok := <-sigCh:
			if !ok {
				return "", fmt.Errorf("bluez signal channel closed while waiting for adapter")
			}
			if !signalIndicatesAdapterUp(sig) {
				continue
			}
			if adapterPath, err := s.findAdapter(); err == nil {
				return adapterPath, nil
			}
		}
	}
}

// registerWithAdapter powers the given adapter and (re-)registers the GATT
// application and advertisement with it. Safe to call again after an adapter
// disappears and comes back — BlueZ drops all registrations for an adapter
// object it removes.
func (s *Server) registerWithAdapter(adapterPath dbus.ObjectPath) error {
	s.adapterMu.Lock()
	s.adapterPath = adapterPath
	s.adapterMu.Unlock()

	if err := s.powerAdapter(adapterPath); err != nil {
		return err
	}

	// Register (or re-register) the pairing agent before the GATT app: the
	// wifi_creds/ap_control/record_control characteristics require an
	// encrypted write, which triggers pairing on the first write from a phone
	// that hasn't bonded yet. Without an agent registered, bluetoothd has
	// nothing to drive Just-Works pairing headlessly and the write fails.
	s.registerPairingAgent()

	adapter := s.conn.Object(bluezName, adapterPath)
	if err := adapter.Call(ifaceGattManager+".RegisterApplication", 0, appPath, map[string]dbus.Variant{}).Err; err != nil {
		return fmt.Errorf("register gatt application: %w", err)
	}

	hciDev := hciDevFromAdapter(string(adapterPath)) // e.g. "/org/bluez/hci0" → "hci0"
	hciAdv := newLegacyAdvertiser(hciDev)
	s.adapterMu.Lock()
	s.hciAdv = hciAdv
	s.adapterMu.Unlock()

	if legacyAdvOnly() {
		// Single advertising path (EQUIP_BLE_LEGACY_ADV, default on): BlueZ's
		// RegisterAdvertisement drives the AIC8800 down its broken extended-adv
		// path, and racing it against the raw legacy HCI advertiser is what
		// left LE_Set_Advertising_Parameters failing with 0x0C (Command
		// Disallowed). Skip RegisterAdvertisement entirely — legacy HCI is the
		// only advertiser.
		if advErr := hciAdv.Start(s.name, serviceUUID); advErr != nil {
			slog.Warn("ble-legacy-adv-start-failed", "error", advErr)
		}
	} else {
		// Old dual-path behavior, kept for comparison/rollback via the env
		// flag: try BlueZ-managed advertisement first, and always also start
		// legacy HCI advertising since on AIC8800D80 BlueZ's extended
		// advertising path sets ActiveInstances=1 but never transmits.
		if regErr := adapter.Call(ifaceLEAdvertisingManager+".RegisterAdvertisement", 0, adPath, map[string]dbus.Variant{}).Err; regErr != nil {
			slog.Warn("ble-bluez-adv-failed-using-legacy-hci", "error", regErr)
		}
		if advErr := hciAdv.Start(s.name, serviceUUID); advErr != nil {
			slog.Warn("ble-legacy-adv-start-failed", "error", advErr)
		}
	}

	s.adapterMu.Lock()
	s.registered = true
	s.adapterMu.Unlock()
	return nil
}

// legacyAdvOnly reports whether legacy-only HCI advertising should be used,
// skipping BlueZ's RegisterAdvertisement entirely. Defaults to true (also set
// explicitly by the companion-net systemd unit): on the AIC8800D80, BlueZ's
// extended-adv RegisterAdvertisement and the raw legacy HCI advertiser both
// drive the radio and race, leaving LE_Set_Advertising_Parameters to fail
// with 0x0C (Command Disallowed). Set EQUIP_BLE_LEGACY_ADV=0 to restore the
// old dual-path behavior.
func legacyAdvOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EQUIP_BLE_LEGACY_ADV"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// watchAdapterEvents runs for the lifetime of the server, reacting to the BT
// adapter (a USB device) disappearing and reappearing — firmware crash, EMI,
// or a bluetoothd restart all invalidate every registration made against it.
func (s *Server) watchAdapterEvents(ctx context.Context, sigCh chan *dbus.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case sig, ok := <-sigCh:
			if !ok {
				return
			}
			s.handleAdapterSignal(sig)
		}
	}
}

func (s *Server) handleAdapterSignal(sig *dbus.Signal) {
	switch sig.Name {
	case memberInterfacesRemoved:
		if len(sig.Body) == 0 {
			return
		}
		path, _ := sig.Body[0].(dbus.ObjectPath)
		if path != "" && path == s.currentAdapterPath() {
			s.handleAdapterLost("bluez-adapter-removed")
		}
	case memberNameOwnerChanged:
		if len(sig.Body) < 3 {
			return
		}
		name, _ := sig.Body[0].(string)
		newOwner, _ := sig.Body[2].(string)
		if name != bluezName {
			return
		}
		if newOwner == "" {
			s.handleAdapterLost("bluez-name-owner-lost")
			return
		}
		s.handleAdapterReappear()
	case memberInterfacesAdded:
		if signalIndicatesAdapterUp(sig) {
			s.handleAdapterReappear()
		}
	}
}

// handleAdapterLost marks the server unregistered without closing s.conn —
// s.conn is our own private bus connection (not shared with network.Manager),
// but it must stay open so future BlueZ signals and the eventual re-register
// still work.
func (s *Server) handleAdapterLost(reason string) {
	s.adapterMu.Lock()
	wasRegistered := s.registered
	s.registered = false
	if s.hciAdv != nil {
		s.hciAdv.Stop()
	}
	s.adapterMu.Unlock()
	if wasRegistered {
		slog.Warn("ble-adapter-lost", "reason", reason)
	}
}

// handleAdapterReappear re-runs the power/register-app/register-advertisement
// sequence once BlueZ reports a usable adapter again.
func (s *Server) handleAdapterReappear() {
	s.adapterMu.Lock()
	alreadyRegistered := s.registered
	s.adapterMu.Unlock()
	if alreadyRegistered {
		return
	}

	adapterPath, err := s.findAdapter()
	if err != nil {
		// Not fully back yet (e.g. GattManager1/LEAdvertisingManager1 not
		// exported yet) — a later InterfacesAdded will retry this.
		return
	}
	if err := s.registerWithAdapter(adapterPath); err != nil {
		slog.Warn("ble-adapter-reregister-failed", "error", err)
		return
	}
	slog.Info("ble-adapter-recovered", "adapter", adapterPath)
}

func (s *Server) currentAdapterPath() dbus.ObjectPath {
	s.adapterMu.Lock()
	defer s.adapterMu.Unlock()
	return s.adapterPath
}

func (s *Server) isStarted() bool {
	s.adapterMu.Lock()
	defer s.adapterMu.Unlock()
	return s.started
}

func (s *Server) setStarted(v bool) {
	s.adapterMu.Lock()
	s.started = v
	s.adapterMu.Unlock()
}

// signalIndicatesAdapterUp reports whether an InterfacesAdded signal body
// includes org.bluez.Adapter1.
func signalIndicatesAdapterUp(sig *dbus.Signal) bool {
	if sig.Name != memberInterfacesAdded || len(sig.Body) < 2 {
		return false
	}
	ifaces, ok := sig.Body[1].(map[string]map[string]dbus.Variant)
	if !ok {
		return false
	}
	_, ok = ifaces[ifaceAdapter]
	return ok
}

// Stop unregisters the app/advertisement and closes the D-Bus connection.
func (s *Server) Stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}

	s.adapterMu.Lock()
	hciAdv := s.hciAdv
	started := s.started
	adapterPath := s.adapterPath
	s.adapterMu.Unlock()

	if hciAdv != nil {
		hciAdv.Stop()
	}
	if started && adapterPath != "" {
		adapter := s.conn.Object(bluezName, adapterPath)
		_ = adapter.Call(ifaceLEAdvertisingManager+".UnregisterAdvertisement", 0, adPath).Err
		_ = adapter.Call(ifaceGattManager+".UnregisterApplication", 0, appPath).Err
	}
	s.conn.Close()
	slog.Info("ble-stopped")
}

func (s *Server) findAdapter() (dbus.ObjectPath, error) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := s.conn.Object(bluezName, "/").Call(ifaceObjectManager+".GetManagedObjects", 0).Store(&objects)
	if err != nil {
		return "", fmt.Errorf("bluez object manager: %w", err)
	}
	for path, ifaces := range objects {
		if _, ok := ifaces[ifaceAdapter]; !ok {
			continue
		}
		if _, ok := ifaces[ifaceGattManager]; !ok {
			continue
		}
		if _, ok := ifaces[ifaceLEAdvertisingManager]; !ok {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("no BlueZ adapter with GATT and LE advertising support found")
}

func (s *Server) powerAdapter(adapterPath dbus.ObjectPath) error {
	obj := s.conn.Object(bluezName, adapterPath)
	if err := obj.Call(ifaceProperties+".Set", 0, ifaceAdapter, "Powered", dbus.MakeVariant(true)).Err; err != nil {
		return fmt.Errorf("power bluetooth adapter: %w", err)
	}
	_ = obj.Call(ifaceProperties+".Set", 0, ifaceAdapter, "Alias", dbus.MakeVariant(s.name)).Err
	return nil
}

func (s *Server) exportObjects() error {
	if err := s.conn.Export(s.app, appPath, ifaceObjectManager); err != nil {
		return err
	}
	for _, obj := range s.app.objects {
		if err := s.conn.Export(obj, obj.Path(), ifaceProperties); err != nil {
			return err
		}
		switch typed := obj.(type) {
		case *gattService:
			if err := s.conn.Export(typed, obj.Path(), ifaceGattService); err != nil {
				return err
			}
		case *characteristic:
			if err := s.conn.Export(typed, obj.Path(), ifaceGattCharacteristic); err != nil {
				return err
			}
		}
	}
	if err := s.conn.Export(s.ad, adPath, ifaceProperties); err != nil {
		return err
	}
	return s.conn.Export(s.ad, adPath, ifaceAdvertisement)
}

func (s *Server) notifyLoop(ctx context.Context) {
	var tickCount int
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-s.ticker.C:
			tickCount++
			if s.status.IsNotifying() {
				s.status.emitValue(s.conn, s.api.StatusPayload(ctx))
			}
			// Re-assert legacy LE advertising every 60s in case BlueZ or a
			// disconnect reset it (AIC8800 quirk: extended adv doesn't transmit).
			if tickCount%12 == 0 {
				s.adapterMu.Lock()
				hciAdv, registered := s.hciAdv, s.registered
				s.adapterMu.Unlock()
				if registered && hciAdv != nil {
					if err := hciAdv.Start(s.name, serviceUUID); err != nil {
						// Promoted from Debug: this is the only in-field signal
						// that advertising has silently died.
						slog.Warn("ble-legacy-adv-refresh-failed", "error", err)
					}
				}
			}
		}
	}
}

// networkResult emits a network result notification. Returns nil (success) for
// non-error states so BLE write handlers don't fail when just emitting status.
func (s *Server) networkResult(state, msg string) *dbus.Error {
	isError := state == "error"
	errField := "null"
	if isError {
		errField = fmt.Sprintf("%q", msg)
	}
	ssidField := "null"
	if !isError && msg != "" {
		ssidField = fmt.Sprintf("%q", msg)
	}
	payload := fmt.Sprintf(`{"state":%q,"ssid":%s,"ip":null,"err":%s}`, state, ssidField, errField)
	s.netres.emitValue(s.conn, []byte(payload))
	if isError {
		return dbus.MakeFailedError(fmt.Errorf("%s", msg))
	}
	return nil
}

// wifiScanPayload returns the cached scan JSON immediately, kicking off a
// background refresh when the cache is empty or stale. Reads must never block
// on the actual ConnMan scan (which can take several seconds) — the generic
// characteristic read timeout in gatt.go is 2s.
func (s *Server) wifiScanPayload() []byte {
	if s.netCtl == nil {
		return []byte(`{"networks":[],"scanning":false,"err":"network controller not available"}`)
	}

	s.scanMu.Lock()
	stale := time.Since(s.scanAt) >= wifiScanCacheTTL
	alreadyScanning := s.scanning
	cached := s.scanResults
	if stale && !alreadyScanning {
		s.scanning = true
	}
	s.scanMu.Unlock()

	if stale && !alreadyScanning {
		go s.refreshWifiScan()
	}

	if cached != nil {
		return cached
	}
	return []byte(`{"networks":[],"scanning":true,"err":null}`)
}

// refreshWifiScan runs a ConnMan scan in the background and caches the
// result. Scans are not bounded by the per-read 2s timeout.
func (s *Server) refreshWifiScan() {
	defer func() {
		s.scanMu.Lock()
		s.scanning = false
		s.scanMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	nets, err := s.netCtl.ScanWifi(ctx)
	if err != nil {
		slog.Warn("ble-wifi-scan-failed", "error", err)
		s.scanMu.Lock()
		s.scanResults = []byte(fmt.Sprintf(`{"networks":[],"scanning":false,"err":%q}`, err.Error()))
		s.scanAt = time.Now()
		s.scanMu.Unlock()
		return
	}

	// Strongest first; cap the list so the BLE payload stays small.
	sort.Slice(nets, func(i, j int) bool { return nets[i].Strength > nets[j].Strength })
	const maxNetworks = 12
	if len(nets) > maxNetworks {
		nets = nets[:maxNetworks]
	}

	payload, err := json.Marshal(map[string]any{
		"networks": nets,
		"scanning": false,
		"err":      nil,
	})
	if err != nil {
		payload = []byte(`{"networks":[],"scanning":false,"err":"marshal failed"}`)
	}

	s.scanMu.Lock()
	s.scanResults = payload
	s.scanAt = time.Now()
	s.scanMu.Unlock()

	slog.Info("ble-wifi-scan-complete", "count", len(nets))
}

func shortName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "equip-1"
	}
	if len(name) <= 18 {
		return name
	}
	return name[:18]
}

// hciDevFromAdapter extracts the HCI device name from a BlueZ adapter D-Bus path.
// E.g. "/org/bluez/hci0" → "hci0".
func hciDevFromAdapter(adapterPath string) string {
	parts := strings.Split(adapterPath, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasPrefix(parts[i], "hci") {
			return parts[i]
		}
	}
	return "hci0"
}
