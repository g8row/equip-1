package httpapi

import (
	"encoding/json"
	"net/http"

	"equip1/companion/server/internal/network"
)

// networkStatus returns a network payload for embedding in /api/status.
func (s *Server) networkStatus() map[string]any {
	if s.network == nil {
		return map[string]any{"mode": "unknown"}
	}
	st, err := s.network.GetStatus()
	if err != nil {
		return map[string]any{"mode": "unknown", "error": err.Error()}
	}
	return networkStatusPayload(st)
}

// handleGetNetwork returns current WiFi/AP status from ConnMan.
func (s *Server) handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	if s.network == nil {
		writeJSON(w, http.StatusOK, map[string]any{"mode": "unknown"})
		return
	}
	status, err := s.network.GetStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, networkStatusPayload(status))
}

// handleSetWifi attempts to join a WiFi network via ConnMan.
func (s *Server) handleSetWifi(w http.ResponseWriter, r *http.Request) {
	if s.network == nil {
		writeError(w, http.StatusNotImplemented, "network manager not available")
		return
	}
	var body struct {
		SSID string `json:"ssid"`
		PSK  string `json:"psk"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SSID == "" {
		writeError(w, http.StatusBadRequest, "ssid required")
		return
	}
	if err := s.network.Connect(r.Context(), body.SSID, body.PSK); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	status, _ := s.network.GetStatus()
	writeJSON(w, http.StatusOK, networkStatusPayload(status))
}

// handleSetAP enables or disables the ConnMan WiFi hotspot.
func (s *Server) handleSetAP(w http.ResponseWriter, r *http.Request) {
	if s.network == nil {
		writeError(w, http.StatusNotImplemented, "network manager not available")
		return
	}
	var body struct {
		Enabled    bool   `json:"enabled"`
		SSID       string `json:"ssid"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.network.SetAP(body.Enabled, body.SSID, body.Passphrase); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	status, _ := s.network.GetStatus()
	writeJSON(w, http.StatusOK, networkStatusPayload(status))
}

// handleScanWifi triggers a WiFi scan and returns visible networks.
func (s *Server) handleScanWifi(w http.ResponseWriter, r *http.Request) {
	if s.network == nil {
		writeJSON(w, http.StatusOK, map[string]any{"networks": []any{}})
		return
	}
	nets, err := s.network.ScanWifi(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if nets == nil {
		nets = []network.WifiNetwork{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": nets})
}

func networkStatusPayload(s network.NetworkStatus) map[string]any {
	mode := "disconnected"
	if s.AP {
		mode = "ap"
	} else if s.State == "online" || s.State == "ready" {
		mode = "connected"
	} else if s.State == "association" || s.State == "configuration" {
		mode = "connecting"
	}
	return map[string]any{
		"mode": mode,
		"ssid": nullableStr(s.SSID),
		"ip":   nullableStr(s.IPv4),
		"ap":   s.AP,
	}
}

func nullableStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}
