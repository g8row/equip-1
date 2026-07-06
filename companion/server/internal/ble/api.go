package ble

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// APIClient talks to companion-api over localhost. companion-net uses it to
// keep BLE record controls independent from the HTTP server's process lifetime.
type APIClient struct {
	base string
	http *http.Client
}

// NewAPIClient constructs a localhost API client.
func NewAPIClient(base string) *APIClient {
	return &APIClient{
		base: trimRightSlash(base),
		http: &http.Client{Timeout: 2 * time.Second},
	}
}

// StatusPayload returns the compact JSON exposed by the BLE status
// characteristic. Keep this small: phones often negotiate a modest BLE MTU.
func (c *APIClient) StatusPayload(ctx context.Context) []byte {
	var status map[string]any
	_ = c.getJSON(ctx, "/api/status", &status)

	var power map[string]any
	_ = c.getJSON(ctx, "/api/system/power", &power)

	stream, _ := status["stream"].(map[string]any)
	recorder, _ := status["recorder"].(map[string]any)
	req, _ := stream["requirements"].(map[string]any)

	rec := false
	if recorder["mode"] == "recording" {
		rec = true
	}

	payload := map[string]any{
		"fw":   boolValue(req["camera_present"]),
		"rec":  rec,
		"ip":   firstLANIPv4(),
		"api":  apiURL(firstLANIPv4()),
		"ssid": nil,
		"ap":   false,
		"bat":  power["battery"],
		"chg":  power["charging"],
		// ap_pass: the device's own hotspot passphrase (network.APPassphrase),
		// so the app reads it here instead of hardcoding a copy — see T4.7.
		"ap_pass": apPassphrase,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"err":"status"}`)
	}
	return data
}

// Record applies a BLE record-control command: 0 stop, 1 start, 2 toggle.
func (c *APIClient) Record(ctx context.Context, command byte) error {
	switch command {
	case 0:
		return c.post(ctx, "/api/record/stop", nil)
	case 1:
		return c.post(ctx, "/api/record/start", nil)
	case 2:
		return c.post(ctx, "/api/record/toggle", nil)
	default:
		return fmt.Errorf("unknown record command: 0x%02x", command)
	}
}

func (c *APIClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *APIClient) post(ctx context.Context, path string, body any) error {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s", path, resp.Status)
	}
	return nil
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func firstLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

func apiURL(ip string) any {
	if ip == "" {
		return nil
	}
	return "http://" + ip + ":8000"
}

func trimRightSlash(v string) string {
	for len(v) > 0 && v[len(v)-1] == '/' {
		v = v[:len(v)-1]
	}
	return v
}
