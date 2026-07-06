// Command companion-net owns BLE provisioning and ConnMan WiFi control.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"equip1/companion/server/internal/ble"
	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/logging"
	"equip1/companion/server/internal/network"
	"equip1/companion/server/internal/provisioning"
)

func main() {
	cfg := config.Load()
	logging.Setup()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	name := os.Getenv("EQUIP_BLE_NAME")
	if name == "" {
		name = "equip-1"
	}

	// Network manager is shared between BLE server and provisioning state machine.
	// D-Bus itself may not be ready yet even though systemd orders us After=dbus.service
	// (ordering doesn't guarantee the bus accepts connections at exec time) — retry
	// before giving up for good.
	var netMgr *network.Manager
	var err error
	for attempt := 1; ; attempt++ {
		netMgr, err = network.NewManager()
		if err == nil {
			break
		}
		if attempt >= 5 {
			slog.Error("companion-net-network-failed", "error", err, "attempts", attempt)
			os.Exit(1)
		}
		slog.Warn("companion-net-network-retrying", "error", err, "attempt", attempt)
		time.Sleep(2 * time.Second)
	}
	defer netMgr.Close()

	provMgr := provisioning.NewManager(netMgr, provisioning.Config{
		APSSID: name,
	})

	go provMgr.Run(ctx) // AP fallback runs no matter what happens to BLE

	server, err := ble.NewServer(ble.Options{
		APIBase: cfg.APIBase,
		Name:    name,
		Prov:    provMgr,
		NetCtl:  netMgr,
	})
	if err != nil {
		// Construction failure (D-Bus object export) — retryable, not fatal.
		slog.Error("companion-net-init-failed", "error", err)
	}

	if server != nil {
		defer server.Stop()
		go func() {
			backoff := 2 * time.Second
			for {
				err := server.Start(ctx)
				if err == nil {
					return // started; adapter-loss recovery is T0.2's job
				}
				slog.Warn("ble-start-failed-retrying", "error", err, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		}()
	}

	<-ctx.Done()
	slog.Info("companion-net-shutdown")
}
