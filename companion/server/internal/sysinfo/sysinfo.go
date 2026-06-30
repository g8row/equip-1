// Package sysinfo exposes host system information such as battery/power state.
package sysinfo

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PowerStatus describes the host's power/battery state. Battery and Charging are
// pointers so they can be JSON null on devices without a battery.
type PowerStatus struct {
	Battery  *int  `json:"battery"`
	Charging *bool `json:"charging"`
}

// Power reads /sys/class/power_supply/* for a battery. On devices without one
// (this hardware) it returns nil battery/charging gracefully.
func Power() PowerStatus {
	base := "/sys/class/power_supply"
	entries, err := os.ReadDir(base)
	if err != nil {
		return PowerStatus{}
	}

	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		typeBytes, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(typeBytes)) != "Battery" {
			continue
		}

		var status PowerStatus
		if capBytes, err := os.ReadFile(filepath.Join(dir, "capacity")); err == nil {
			if pct, err := strconv.Atoi(strings.TrimSpace(string(capBytes))); err == nil {
				status.Battery = &pct
			}
		}
		if statBytes, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			s := strings.TrimSpace(string(statBytes))
			charging := s == "Charging" || s == "Full"
			status.Charging = &charging
		}
		return status
	}

	return PowerStatus{}
}
