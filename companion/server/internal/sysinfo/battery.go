package sysinfo

// BatteryStatus represents the battery and charging state.
type BatteryStatus struct {
	Percent  *int  `json:"percent"`
	Charging *bool `json:"charging"`
}

// GetBatteryStatus returns the current battery status.
// On this specific RK3528 board without battery sysfs, it returns nulls.
func GetBatteryStatus() BatteryStatus {
	return BatteryStatus{
		Percent:  nil,
		Charging: nil,
	}
}
