package ble

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/godbus/dbus/v5"
)

const (
	ifaceAgentManager = "org.bluez.AgentManager1"
	ifaceAgent        = "org.bluez.Agent1"

	agentPath dbus.ObjectPath = "/com/equip1/companion/pairing_agent"
	// agentCapability declares no input and no output to bluetoothd, so
	// bonding with a phone (also NoInputNoOutput/DisplayYesNo-with-no-display,
	// as virtually all phones are) uses Just Works pairing: no PIN/passkey
	// prompt on either side. This is required for the encrypt-write
	// characteristics (T4.7) to work headlessly — without any agent
	// registered, bluetoothd has nothing to drive pairing and the triggering
	// write fails outright.
	agentCapability = "NoInputNoOutput"
)

// pairingAgent implements org.bluez.Agent1. Every method that could
// legitimately be invoked for a NoInputNoOutput agent accepts unconditionally
// (Just Works semantics: there is no display to show a passkey against or
// keypad to enter one on). See doc/agent-api.txt in the BlueZ source for the
// interface contract.
//
// Risk note: Just Works pairing authenticates that *a* device is present, not
// *which* device — it is not MITM-proof. That's an accepted risk for this
// product (see companion/README.md); an app-layer ECDH exchange would be the
// upgrade path if a stronger guarantee is ever needed.
type pairingAgent struct{}

func (a *pairingAgent) Release() *dbus.Error { return nil }

func (a *pairingAgent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	return "", dbus.MakeFailedError(fmt.Errorf("no input capability"))
}

func (a *pairingAgent) DisplayPinCode(device dbus.ObjectPath, pincode string) *dbus.Error {
	return nil
}

func (a *pairingAgent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error) {
	return 0, dbus.MakeFailedError(fmt.Errorf("no input capability"))
}

func (a *pairingAgent) DisplayPasskey(device dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	return nil
}

func (a *pairingAgent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) *dbus.Error {
	slog.Info("ble-pairing-confirm", "device", device)
	return nil
}

func (a *pairingAgent) RequestAuthorization(device dbus.ObjectPath) *dbus.Error {
	slog.Info("ble-pairing-authorize", "device", device)
	return nil
}

func (a *pairingAgent) AuthorizeService(device dbus.ObjectPath, uuid string) *dbus.Error {
	return nil
}

func (a *pairingAgent) Cancel() *dbus.Error { return nil }

// registerPairingAgent exports the pairing agent and registers+defaults it
// with bluetoothd. It is safe to call repeatedly (e.g. every time an adapter
// (re)registers): re-exporting the same path/interface just replaces the
// handler, and "already exists" from RegisterAgent (bluetoothd restarted vs.
// merely the adapter bouncing) is expected and logged quietly rather than as
// a warning.
func (s *Server) registerPairingAgent() {
	if err := s.conn.Export(&pairingAgent{}, agentPath, ifaceAgent); err != nil {
		slog.Warn("ble-agent-export-failed", "error", err)
		return
	}

	bluez := s.conn.Object(bluezName, "/org/bluez")
	if err := bluez.Call(ifaceAgentManager+".RegisterAgent", 0, agentPath, agentCapability).Err; err != nil {
		if !isBluezAlreadyExists(err) {
			slog.Warn("ble-agent-register-failed", "error", err)
			return
		}
		slog.Debug("ble-agent-already-registered")
	}

	if err := bluez.Call(ifaceAgentManager+".RequestDefaultAgent", 0, agentPath).Err; err != nil {
		slog.Warn("ble-agent-default-failed", "error", err)
	}
}

func isBluezAlreadyExists(err error) bool {
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		return dbusErr.Name == "org.bluez.Error.AlreadyExists"
	}
	return false
}
