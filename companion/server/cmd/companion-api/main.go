// Command companion-api is the Go port of the equip-1 companion FastAPI backend.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/files"
	"equip1/companion/server/internal/httpapi"
	"equip1/companion/server/internal/logging"
	"equip1/companion/server/internal/network"
	"equip1/companion/server/internal/recorder"
	"equip1/companion/server/internal/stream"
)

func main() {
	cfg := config.Load()
	logging.Setup(cfg.LogFile)

	// ---- Singletons ----
	netMgr, netErr := network.NewManager()
	if netErr != nil {
		slog.Warn("network-manager-unavailable", "error", netErr)
		netMgr = nil
	} else {
		defer netMgr.Close()
	}

	captureMode := config.NewCaptureMode(cfg.StartupMode)
	mediamtx := stream.NewMediamtxManager(cfg.MediamtxBinary)
	broadcaster := stream.NewMjpegBroadcaster(cfg.MediamtxRTSPURL)
	seamless := stream.NewSeamlessDvHub(mediamtx, cfg.MediamtxRTSPURL)
	directMjpeg := stream.NewDirectMjpegManager()
	preview := stream.NewPreviewPush(captureMode, mediamtx, cfg.MediamtxRTSPURL)
	fileStore := files.New(cfg.CaptureDir)

	rec := recorder.New(recorder.Deps{
		CaptureDir:         cfg.CaptureDir,
		CaptureMode:        captureMode,
		Mediamtx:           mediamtx,
		MediamtxBinary:     cfg.MediamtxBinary,
		Seamless:           seamless,
		Preview:            preview,
		RTSPURL:            cfg.MediamtxRTSPURL,
		StopAllDirectMjpeg: directMjpeg.StopAll,
	})
	// Wire preview's recording-state predicate (mirrors main.py setting
	// preview._recorder after construction).
	preview.SetRecordingCheck(rec.IsRecording)

	srv := httpapi.New(httpapi.Deps{
		Config:      cfg,
		CaptureMode: captureMode,
		Mediamtx:    mediamtx,
		Broadcaster: broadcaster,
		Seamless:    seamless,
		Preview:     preview,
		DirectMjpeg: directMjpeg,
		Recorder:    rec,
		Files:       fileStore,
		Network:     netMgr,
	})

	// ---- Startup ----
	slog.Info("startup-begin")
	mediamtx.Start()
	slog.Info("startup-complete")

	httpServer := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", cfg.APIPort),
		Handler: srv.Handler(),
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("http-listen", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// ---- Graceful shutdown ----
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("http-server-error", "error", err)
	case <-stop:
		slog.Warn("shutdown-begin")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)

	// Stop all workers (mirrors on_shutdown ordering).
	seamless.Stop()
	rec.StopMjpegFanout()
	broadcaster.Stop()
	preview.Stop()
	directMjpeg.StopAll()
	rec.Stop()
	mediamtx.Stop()
	slog.Warn("shutdown-complete")
}
