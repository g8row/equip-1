package provisioning

import (
	"context"
	"log/slog"
	"time"

	"equip1/companion/server/internal/network"
)

type Config struct {
	FallbackTimeout time.Duration
	APSSID          string
	APPassphrase    string
}

type Manager struct {
	net    *network.Manager
	config Config
}

func NewManager(net *network.Manager, cfg Config) *Manager {
	if cfg.FallbackTimeout == 0 {
		cfg.FallbackTimeout = 30 * time.Second
	}
	if cfg.APSSID == "" {
		cfg.APSSID = "equip-1"
	}
	if cfg.APPassphrase == "" {
		cfg.APPassphrase = network.APPassphrase
	}
	return &Manager{
		net:    net,
		config: cfg,
	}
}

// Run starts the provisioning state machine loop.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Initial wait to let connman auto-connect to known networks
	time.Sleep(5 * time.Second)

	disconnectedSince := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := m.net.GetStatus()
			if err != nil {
				slog.Error("provisioning: get status error", "error", err)
				continue
			}

			if status.State == "ready" || status.State == "online" || status.AP {
				// We are connected or AP is running
				disconnectedSince = time.Time{}
				continue
			}

			// We are disconnected
			if disconnectedSince.IsZero() {
				disconnectedSince = time.Now()
			} else if time.Since(disconnectedSince) >= m.config.FallbackTimeout {
				slog.Info("provisioning: network timeout, falling back to AP mode", "ssid", m.config.APSSID)
				if err := m.net.SetAP(true, m.config.APSSID, m.config.APPassphrase); err != nil {
					slog.Error("provisioning: failed to start AP", "error", err)
				}
				disconnectedSince = time.Time{} // Reset so we don't spam
			}
		}
	}
}

// ApplyCredentials attempts to connect to a new WiFi network.
func (m *Manager) ApplyCredentials(ctx context.Context, ssid, passphrase string) error {
	slog.Info("provisioning: applying credentials", "ssid", ssid)
	
	// Disable AP mode if it's on
	_ = m.net.SetAP(false, "", "")

	// Give connman a moment to settle after disabling AP
	time.Sleep(2 * time.Second)

	err := m.net.Connect(ctx, ssid, passphrase)
	if err != nil {
		slog.Error("provisioning: connect failed, falling back to AP", "error", err)
		_ = m.net.SetAP(true, m.config.APSSID, m.config.APPassphrase)
		return err
	}

	return nil
}
