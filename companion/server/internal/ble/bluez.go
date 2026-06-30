package ble

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	bluezName = "org.bluez"

	ifaceObjectManager        = "org.freedesktop.DBus.ObjectManager"
	ifaceProperties           = "org.freedesktop.DBus.Properties"
	ifaceAdapter              = "org.bluez.Adapter1"
	ifaceGattManager          = "org.bluez.GattManager1"
	ifaceLEAdvertisingManager = "org.bluez.LEAdvertisingManager1"
	ifaceGattService          = "org.bluez.GattService1"
	ifaceGattCharacteristic   = "org.bluez.GattCharacteristic1"
	ifaceAdvertisement        = "org.bluez.LEAdvertisement1"

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
)

// Provisioner is satisfied by provisioning.Manager; kept as an interface to
// avoid an import cycle.
type Provisioner interface {
	ApplyCredentials(ctx context.Context, ssid, passphrase string) error
}

// NetworkController is satisfied by network.Manager.
type NetworkController interface {
	SetAP(enable bool, ssid, passphrase string) error
}

// Server owns the BlueZ advertisement and local GATT application.
type Server struct {
	conn        *dbus.Conn
	api         *APIClient
	name        string
	adapterPath dbus.ObjectPath
	prov        Provisioner
	netCtl      NetworkController

	app     *application
	ad      *advertisement
	hciAdv  *legacyAdvertiser
	status  *characteristic
	netres  *characteristic
	ticker  *time.Ticker
	done    chan struct{}
	started bool
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
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
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
	wifiCreds := newCharacteristic(wifiCredsUUID, servicePath, "wifi_creds", []string{"write", "encrypt-write"})
	apControl := newCharacteristic(apControlUUID, servicePath, "ap_control", []string{"write"})
	recordControl := newCharacteristic(recordControlUUID, servicePath, "record_control", []string{"write"})
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
		if err := s.netCtl.SetAP(enable, s.name, ""); err != nil {
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
		return s.netres.value, nil
	}
	wifiScan.read = func(ctx context.Context) ([]byte, *dbus.Error) {
		return []byte(`{"networks":[],"err":"scan-not-implemented"}`), nil
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
	s.netres.value = []byte(`{"state":"idle","ssid":null,"ip":null,"err":null}`)
}

// Start powers the adapter, exports the GATT objects and registers with BlueZ.
func (s *Server) Start(ctx context.Context) error {
	if s.started {
		return nil
	}
	adapterPath, err := s.findAdapter()
	if err != nil {
		return err
	}
	s.adapterPath = adapterPath
	if err := s.powerAdapter(); err != nil {
		return err
	}
	if err := s.exportObjects(); err != nil {
		return err
	}

	adapter := s.conn.Object(bluezName, adapterPath)
	if err := adapter.Call(ifaceGattManager+".RegisterApplication", 0, appPath, map[string]dbus.Variant{}).Err; err != nil {
		return fmt.Errorf("register gatt application: %w", err)
	}

	// Try BlueZ-managed advertisement first; if it fails (e.g. AIC8800 does not
	// support extended LE advertising commands), fall back to raw HCI legacy
	// advertising via hcitool.  The GATT service still handles incoming connections
	// regardless of which advertising path is used.
	hciDev := hciDevFromAdapter(string(adapterPath)) // e.g. "/org/bluez/hci0" → "hci0"
	s.hciAdv = newLegacyAdvertiser(hciDev)

	regErr := adapter.Call(ifaceLEAdvertisingManager+".RegisterAdvertisement", 0, adPath, map[string]dbus.Variant{}).Err
	if regErr != nil {
		slog.Warn("ble-bluez-adv-failed-using-legacy-hci", "error", regErr)
	}
	// Always also start legacy HCI advertising so the chip actually transmits —
	// on AIC8800D80 BlueZ's extended advertising path sets ActiveInstances=1
	// but never transmits; legacy HCI commands are confirmed to work.
	if advErr := s.hciAdv.Start(s.name, serviceUUID); advErr != nil {
		slog.Warn("ble-legacy-adv-start-failed", "error", advErr)
	}

	if regErr != nil && s.hciAdv == nil {
		_ = adapter.Call(ifaceGattManager+".UnregisterApplication", 0, appPath).Err
		return fmt.Errorf("register advertisement: %w", regErr)
	}

	s.started = true
	s.ticker = time.NewTicker(5 * time.Second)
	go s.notifyLoop(ctx)
	slog.Info("ble-started", "adapter", adapterPath, "name", s.name, "service_uuid", serviceUUID)
	return nil
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
	if s.hciAdv != nil {
		s.hciAdv.Stop()
	}
	if s.started && s.adapterPath != "" {
		adapter := s.conn.Object(bluezName, s.adapterPath)
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

func (s *Server) powerAdapter() error {
	obj := s.conn.Object(bluezName, s.adapterPath)
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
			if s.status.notifying {
				s.status.emitValue(s.conn, s.api.StatusPayload(ctx))
			}
			// Re-assert legacy LE advertising every 60s in case BlueZ or a
			// disconnect reset it (AIC8800 quirk: extended adv doesn't transmit).
			if tickCount%12 == 0 && s.hciAdv != nil {
				if err := s.hciAdv.Start(s.name, serviceUUID); err != nil {
					slog.Debug("ble-legacy-adv-refresh-failed", "error", err)
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
