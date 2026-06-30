// Package network reports connectivity status. This is a minimal placeholder;
// the real ConnMan-backed implementation arrives in a later task.
package network

// Status describes the device's network state.
type Status struct {
	Mode string `json:"mode"`
}

// GetStatus returns the current network status. For now it always reports
// "unknown".
func GetStatus() Status {
	return Status{Mode: "unknown"}
}
