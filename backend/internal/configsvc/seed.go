package configsvc

import (
	"errors"
	"log/slog"
	"os"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

// SeedFromConfig imports a legacy config.yaml into the store exactly once: only
// when the store has no services yet and the file exists and parses. It lets an
// existing file-based deployment carry its vendors, keys, and prices over to the
// SQLite-owned model on first boot. Thereafter the file is ignored (the
// dashboard is the source of truth); it can still be re-imported manually.
//
// It returns the number of services imported (0 when skipped).
func SeedFromConfig(st *store.Store, configPath string, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	n, err := st.CountServices()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil // already managing services; never clobber
	}

	snap, err := config.LoadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // nothing to seed from
		}
		logger.Warn("config seed skipped: file did not parse", "path", configPath, "err", err)
		return 0, nil
	}

	// Carry settings over too, so capture behavior is preserved.
	settings := snap.Settings()
	if err := st.UpdateAppSettings(store.AppSettings{
		Capture:         settings.Capture,
		CaptureMaxBytes: settings.CaptureMaxBytes,
		CaptureRetain:   settings.CaptureRetain,
	}); err != nil {
		logger.Warn("config seed: failed to import settings", "err", err)
	}

	imported := 0
	for _, v := range snap.Vendors() {
		keys := make([]string, 0, len(v.Credentials))
		for _, c := range v.Credentials {
			keys = append(keys, c.APIKey)
		}

		models := make([]store.ServiceModel, 0, len(v.ServedModels))
		for _, m := range v.ServedModels {
			sm := store.ServiceModel{Model: m, Unit: "per_1m_tokens"}
			if p, ok := v.Prices[m]; ok {
				sm.Input, sm.Output = p.Input, p.Output
				if p.Unit != "" {
					sm.Unit = p.Unit
				}
			}
			models = append(models, sm)
		}

		if _, err := st.CreateService(store.NewService{
			Name:     v.Name,
			Adapter:  v.Adapter,
			BaseURL:  v.BaseURL,
			Priority: v.Priority,
			Weight:   v.Weight,
			Enabled:  true,
			APIKeys:  keys,
			Models:   models,
		}); err != nil {
			logger.Warn("config seed: failed to import vendor", "vendor", v.Name, "err", err)
			continue
		}
		imported++
	}

	if imported > 0 {
		logger.Info("imported vendors from config.yaml into the store", "path", configPath, "count", imported)
	}
	return imported, nil
}
