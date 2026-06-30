// Command companion-net owns BLE provisioning and ConnMan WiFi control.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
	netMgr, err := network.NewManager()
	if err != nil {
		slog.Error("companion-net-network-failed", "error", err)
		os.Exit(1)
	}
	defer netMgr.Close()

	provMgr := provisioning.NewManager(netMgr, provisioning.Config{
		APSSID: name,
	})

	server, err := ble.NewServer(ble.Options{
		APIBase: cfg.APIBase,
		Name:    name,
		Prov:    provMgr,
		NetCtl:  netMgr,
	})
	if err != nil {
		slog.Error("companion-net-init-failed", "error", err)
		os.Exit(1)
	}
	defer server.Stop()

	if err := server.Start(ctx); err != nil {
		slog.Error("companion-net-start-failed", "error", err)
		os.Exit(1)
	}

	go provMgr.Run(ctx)

	<-ctx.Done()
	slog.Info("companion-net-shutdown")
}
