package ble

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// legacyAdvertiser sets up LE advertising via raw HCI commands (hcitool).
//
// The AIC8800D80 chip via mainline btusb reports BT 5.4 but the firmware
// does not implement BT5 extended LE commands.  BlueZ therefore fails when
// it tries to use LE_Set_Ext_Adv_* commands.  Legacy LE_Set_Advertising_*
// commands (pre-BT5) work correctly on this chip.
//
// We call these directly via hcitool after registering the GATT application
// with BlueZ, bypassing BlueZ's advertisement manager for the actual radio
// advertisement.  BlueZ still handles incoming GATT connections.
type legacyAdvertiser struct {
	hciDev string // e.g. "hci0"
}

func newLegacyAdvertiser(hciDev string) *legacyAdvertiser {
	return &legacyAdvertiser{hciDev: hciDev}
}

// Start configures and enables LE advertising with the given device name and
// 128-bit service UUID.  name is truncated to 7 chars to fit the 31-byte
// primary advertisement packet alongside flags.  The full 128-bit service UUID
// is placed in the scan-response packet.
func (a *legacyAdvertiser) Start(name, svcUUID string) error {
	if _, err := exec.LookPath("hcitool"); err != nil {
		return fmt.Errorf("hcitool not found — cannot set up legacy LE advertising")
	}

	// Build primary advertising data (max 31 significant bytes):
	//   Flags:       02 01 06  (LE general discoverable, BR/EDR not supported)
	//   Local name:  [len] 09 [name bytes]
	nameBytes := []byte(name)
	if len(nameBytes) > 19 {
		nameBytes = nameBytes[:19]
	}
	// AD entry: length byte + type (0x09) + name
	nameAD := append([]byte{byte(len(nameBytes) + 1), 0x09}, nameBytes...)
	flagsAD := []byte{0x02, 0x01, 0x06}
	advData := append(flagsAD, nameAD...)

	// Build scan-response data: 128-bit service UUID in little-endian
	uuidLE, err := uuid128LE(svcUUID)
	if err != nil {
		return fmt.Errorf("encode service uuid: %w", err)
	}
	// AD entry: length=17 (1 type + 16 UUID bytes), type=0x07 (complete 128-bit UUIDs)
	svcAD := append([]byte{0x11, 0x07}, uuidLE...)

	// Disable advertising before reconfiguring (status 0x0C = Command Disallowed
	// if parameters are changed while advertising is active).
	_ = a.hciCmd("0x08", "0x000A", "00")

	if err := a.hciCmd("0x08", "0x0006",
		// LE_Set_Advertising_Parameters:
		// interval min/max 160 (100ms), ADV_IND, own addr public,
		// peer addr type public, peer addr 00:00:00:00:00:00,
		// channel map all (37+38+39), filter policy none
		"A0", "00", "A0", "00", "00",
		"00", "00", "00", "00", "00", "00", "00", "00", "07", "00",
	); err != nil {
		return fmt.Errorf("set adv params: %w", err)
	}

	if err := a.hciCmd("0x08", "0x0008", buildAdvPayload(advData)...); err != nil {
		return fmt.Errorf("set adv data: %w", err)
	}

	if err := a.hciCmd("0x08", "0x0009", buildAdvPayload(svcAD)...); err != nil {
		return fmt.Errorf("set scan rsp: %w", err)
	}

	if err := a.hciCmd("0x08", "0x000A", "01"); err != nil {
		return fmt.Errorf("enable adv: %w", err)
	}

	slog.Info("ble-legacy-adv-started", "device", a.hciDev, "name", name, "svc_uuid", svcUUID)
	return nil
}

// Stop disables LE advertising.
func (a *legacyAdvertiser) Stop() {
	_ = a.hciCmd("0x08", "0x000A", "00")
}

// hciCmd runs: hcitool -i <dev> cmd <ogf> <ocf> [params...]
func (a *legacyAdvertiser) hciCmd(ogf, ocf string, params ...string) error {
	args := append([]string{"-i", a.hciDev, "cmd", ogf, ocf}, params...)
	out, err := exec.Command("hcitool", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("hcitool %s %s: %v (%s)", ogf, ocf, err, strings.TrimSpace(string(out)))
	}
	// Check for error status in response: "> HCI Event: ... XX" where XX != 00
	outStr := string(out)
	if strings.Contains(outStr, "> HCI Event:") {
		lines := strings.Split(outStr, "\n")
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "05") {
				// Response line: "05 XX YY SS" — SS is status at index 3
				fields := strings.Fields(strings.TrimSpace(l))
				if len(fields) >= 4 && fields[3] != "00" {
					return fmt.Errorf("hcitool %s %s: status 0x%s", ogf, ocf, fields[3])
				}
			}
		}
	}
	return nil
}

// buildAdvPayload returns the 32-byte HCI payload for LE_Set_Advertising_Data
// or LE_Set_Scan_Response_Data: [length, data..., padding...].
func buildAdvPayload(data []byte) []string {
	if len(data) > 31 {
		data = data[:31]
	}
	payload := make([]string, 32)
	payload[0] = fmt.Sprintf("%02X", len(data))
	for i, b := range data {
		payload[i+1] = fmt.Sprintf("%02X", b)
	}
	for i := len(data) + 1; i < 32; i++ {
		payload[i] = "00"
	}
	return payload
}

// uuid128LE converts "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" to little-endian bytes.
func uuid128LE(uuid string) ([]byte, error) {
	clean := strings.ReplaceAll(uuid, "-", "")
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != 16 {
		return nil, fmt.Errorf("invalid 128-bit UUID: %s", uuid)
	}
	// Reverse to little-endian
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}
