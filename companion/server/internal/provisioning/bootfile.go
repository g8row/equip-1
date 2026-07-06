package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BootProvisionEnvVar overrides the default boot-partition provisioning file
// path. Primarily useful for testing and for hardware where the FAT boot
// partition is mounted somewhere other than /boot.
const BootProvisionEnvVar = "EQUIP_BOOT_PROVISION_PATH"

// DefaultBootProvisionPath is where a laptop can drop a provisioning file
// (`{"ssid": "...", "psk": "..."}`) onto the FAT boot partition before the
// board's first boot — the accessibility-class fallback for phones/BLE not
// being an option at all (T0.7). NOTE: this requires the boot partition to
// actually be mounted at this path on the running rootfs; wiring that mount
// (a systemd .mount unit or fstab entry) is tracked separately, not part of
// this loader.
const DefaultBootProvisionPath = "/boot/provision.json"

// bootProvisionMaxAttempts caps ApplyCredentials retries for a single boot
// file. The file is renamed away after this many attempts regardless of
// outcome so a bad password or an unreachable network can't turn every
// future boot into a repeated multi-second connect attempt.
const bootProvisionMaxAttempts = 3

// bootProvisionRetryDelay is the pause between ApplyCredentials attempts.
var bootProvisionRetryDelay = 3 * time.Second

type bootCredentials struct {
	SSID string `json:"ssid"`
	PSK  string `json:"psk"`
}

// credentialApplier is satisfied by *Manager; declared as a narrow interface
// so tests can supply a fake without a real network.Manager/D-Bus connection.
type credentialApplier interface {
	ApplyCredentials(ctx context.Context, ssid, passphrase string) error
}

// BootProvisionPath returns the configured boot-partition provisioning file
// path: BootProvisionEnvVar if set, else DefaultBootProvisionPath.
func BootProvisionPath() string {
	if p := os.Getenv(BootProvisionEnvVar); p != "" {
		return p
	}
	return DefaultBootProvisionPath
}

// ApplyBootProvisionFile looks for a provisioning file at path and, if
// present, applies it via m.ApplyCredentials and renames it to
// "<name>.applied<ext>" so it is never re-applied. FAT has no file
// permissions to lock the file down once used, so the rename — not
// deletion — is what makes re-provisioning deliberate; the file should still
// be deleted from the boot partition after use as a matter of hygiene.
//
// The rename happens even when parsing or applying fails (after
// bootProvisionMaxAttempts), so a malformed file or a bad/unreachable
// network can't wedge every future boot in the same failed attempt. The
// outcome (success, parse failure, or apply failure) is always logged.
//
// A missing file is not an error and is not logged — most boots have no
// boot-partition provisioning file at all.
func ApplyBootProvisionFile(ctx context.Context, m credentialApplier, path string) error {
	data, readErr := os.ReadFile(path)
	if readErr != nil && errors.Is(readErr, os.ErrNotExist) {
		return nil
	}

	var creds bootCredentials
	var outcomeErr error
	switch {
	case readErr != nil:
		outcomeErr = fmt.Errorf("read %s: %w", path, readErr)
	default:
		if err := json.Unmarshal(data, &creds); err != nil {
			outcomeErr = fmt.Errorf("parse %s: %w", path, err)
		} else if creds.SSID == "" {
			outcomeErr = fmt.Errorf("%s: missing \"ssid\" field", path)
		}
	}

	if outcomeErr == nil {
		outcomeErr = applyBootCredentialsWithRetry(ctx, m, creds.SSID, creds.PSK)
	}

	appliedPath := appliedBootProvisionPath(path)
	if renameErr := os.Rename(path, appliedPath); renameErr != nil {
		slog.Error("boot-provision-rename-failed", "path", path, "target", appliedPath, "error", renameErr)
		if outcomeErr == nil {
			outcomeErr = renameErr
		}
		return outcomeErr
	}

	if outcomeErr == nil {
		slog.Info("boot-provision-applied", "ssid", creds.SSID, "renamed_to", appliedPath)
	} else {
		slog.Warn("boot-provision-failed", "path", path, "error", outcomeErr, "renamed_to", appliedPath)
	}
	return outcomeErr
}

// applyBootCredentialsWithRetry calls m.ApplyCredentials up to
// bootProvisionMaxAttempts times, pausing bootProvisionRetryDelay between
// attempts (aborting early if ctx is canceled).
func applyBootCredentialsWithRetry(ctx context.Context, m credentialApplier, ssid, psk string) error {
	var lastErr error
	for attempt := 1; attempt <= bootProvisionMaxAttempts; attempt++ {
		lastErr = m.ApplyCredentials(ctx, ssid, psk)
		if lastErr == nil {
			return nil
		}
		slog.Warn("boot-provision-apply-attempt-failed", "ssid", ssid, "attempt", attempt, "error", lastErr)
		if attempt < bootProvisionMaxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(bootProvisionRetryDelay):
			}
		}
	}
	return lastErr
}

// appliedBootProvisionPath turns "/boot/provision.json" into
// "/boot/provision.applied.json" (or "<path>.applied" if path has no
// extension).
func appliedBootProvisionPath(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return base + ".applied" + ext
}
