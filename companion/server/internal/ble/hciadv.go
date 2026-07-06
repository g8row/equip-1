package ble

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// hciCmdTimeout bounds every hcitool invocation so a wedged HCI controller
// cannot stall the caller — notifyLoop calls into Start synchronously every
// 60s to refresh advertising, and a hung hcitool would otherwise block that
// forever.
const hciCmdTimeout = 3 * time.Second

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
// 128-bit service UUID.
//
// The 128-bit service UUID goes in the PRIMARY advertising packet, not the
// scan-response: Android's ScanFilter.setServiceUuid (which the companion app
// uses) matches against the primary ADV data, and a UUID placed only in the
// scan-response is silently missed by filtered scans — that was the cause of
// flaky/failed discovery. Layout of the 31-byte primary packet:
//   Flags (3) + complete 128-bit service UUID (18) + as much of the name as
//   fits (2 + nameLen). For "equip-1" that's 3+18+9 = 30 bytes. The full name
//   also goes in the scan-response so longer names still resolve.
func (a *legacyAdvertiser) Start(name, svcUUID string) error {
	if _, err := exec.LookPath("hcitool"); err != nil {
		return fmt.Errorf("hcitool not found — cannot set up legacy LE advertising")
	}

	uuidLE, err := uuid128LE(svcUUID)
	if err != nil {
		return fmt.Errorf("encode service uuid: %w", err)
	}

	nameBytes := []byte(name)

	// Primary ADV: flags + 128-bit service UUID (type 0x07), then the name if it
	// still fits within the 31-byte budget.
	flagsAD := []byte{0x02, 0x01, 0x06}          // 3 bytes
	svcAD := append([]byte{0x11, 0x07}, uuidLE...) // 18 bytes (len 17 + this byte)
	advData := append(append([]byte{}, flagsAD...), svcAD...)
	if room := 31 - len(advData) - 2; room > 0 {
		primaryName := nameBytes
		if len(primaryName) > room {
			primaryName = primaryName[:room]
		}
		advData = append(advData, append([]byte{byte(len(primaryName) + 1), 0x09}, primaryName...)...)
	}

	// Scan-response: the full device name (0x09), so longer names are still
	// readable even when truncated in the primary packet.
	scanName := nameBytes
	if len(scanName) > 29 {
		scanName = scanName[:29]
	}
	scanRspData := append([]byte{byte(len(scanName) + 1), 0x09}, scanName...)

	// Disable advertising before reconfiguring (status 0x0C = Command Disallowed
	// if parameters are changed while advertising is active).
	_, _ = a.hciCmd("0x08", "0x000A", "00")

	paramsStatus, err := a.hciCmd("0x08", "0x0006",
		// LE_Set_Advertising_Parameters:
		// interval min/max 160 (100ms), ADV_IND, own addr public,
		// peer addr type public, peer addr 00:00:00:00:00:00,
		// channel map all (37+38+39), filter policy none
		"A0", "00", "A0", "00", "00",
		"00", "00", "00", "00", "00", "00", "00", "00", "07", "00",
	)
	// Logged unconditionally (success or failure): this status is the field
	// signature of the AIC8800D80 defect — 0x0C (Command Disallowed) means
	// something else (BlueZ's extended-adv path, or a leftover advertiser
	// from a previous run) is still driving the radio.
	slog.Info("ble-legacy-adv-params-status", "device", a.hciDev, "status", fmt.Sprintf("0x%02X", paramsStatus))
	if err != nil {
		return fmt.Errorf("set adv params: %w", err)
	}

	if _, err := a.hciCmd("0x08", "0x0008", buildAdvPayload(advData)...); err != nil {
		return fmt.Errorf("set adv data: %w", err)
	}

	if _, err := a.hciCmd("0x08", "0x0009", buildAdvPayload(scanRspData)...); err != nil {
		return fmt.Errorf("set scan rsp: %w", err)
	}

	if _, err := a.hciCmd("0x08", "0x000A", "01"); err != nil {
		return fmt.Errorf("enable adv: %w", err)
	}

	slog.Info("ble-legacy-adv-started", "device", a.hciDev, "name", name, "svc_uuid", svcUUID)
	return nil
}

// Stop disables LE advertising.
func (a *legacyAdvertiser) Stop() {
	_, _ = a.hciCmd("0x08", "0x000A", "00")
}

// hciCmd runs `hcitool -i <dev> cmd <ogf> <ocf> [params...]`, bounded by
// hciCmdTimeout, and returns the parsed Command Complete status byte
// (0x00 = success).
//
// hcitool's output for a Command Complete event looks like:
//
//	> HCI Event: 0x0e plen 4
//	01 06 20 00
//
// The four bytes on the line following the "0x0e" event header are
// num_hci_command_packets, opcode-lo, opcode-hi, status — the LAST byte is
// the status this parses. (The previous parser looked for a line starting
// with "05", which hcitool never emits for these commands — it never
// matched, so a nonzero status such as 0x0C Command Disallowed was silently
// swallowed as success.)
func (a *legacyAdvertiser) hciCmd(ogf, ocf string, params ...string) (byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hciCmdTimeout)
	defer cancel()

	args := append([]string{"-i", a.hciDev, "cmd", ogf, ocf}, params...)
	out, err := exec.CommandContext(ctx, "hcitool", args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("hcitool %s %s: %v (%s)", ogf, ocf, err, strings.TrimSpace(string(out)))
	}

	lines := strings.Split(string(out), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "HCI Event: 0x0e") {
			continue
		}
		if i+1 >= len(lines) {
			break
		}
		fields := strings.Fields(strings.TrimSpace(lines[i+1]))
		if len(fields) == 0 {
			break
		}
		status, perr := strconv.ParseUint(fields[len(fields)-1], 16, 8)
		if perr != nil {
			break
		}
		if status != 0 {
			return byte(status), fmt.Errorf("hcitool %s %s: status 0x%02X", ogf, ocf, status)
		}
		return byte(status), nil
	}
	// No Command Complete event found in the output (e.g. hcitool emitted a
	// Command Status event instead) — nothing to report as an error, but log
	// for diagnostics since we can't confirm success either.
	slog.Debug("hcitool-no-command-complete-event", "ogf", ogf, "ocf", ocf, "output", strings.TrimSpace(string(out)))
	return 0, nil
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
