package main

// config.toml is re-read before each cast.
//
// The daemon used to read its configuration once, at start, and lives up to
// fifteen minutes past the last cast: a changed frame rate or GPU setting only
// took effect after `castr quit`. Now every cast start checks the file and, if
// it changed, reloads it, so `castr set` and the bar's settings apply to the
// next cast. A cast already running keeps what it started with.

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/discovery"
)

type liveConfig struct {
	path     string
	onChange func(config.Config)

	mu      sync.Mutex
	cfg     config.Config
	modTime time.Time
	size    int64
	exists  bool
}

func newLiveConfig(path string, initial config.Config) *liveConfig {
	l := &liveConfig{path: path, cfg: initial}
	if info, err := os.Stat(path); err == nil {
		l.modTime, l.size, l.exists = info.ModTime(), info.Size(), true
	}
	return l
}

func (l *liveConfig) current() config.Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

// refresh reloads the file if it changed since the last look. A file with an
// error keeps the configuration in use: a typo must not reset every setting.
func (l *liveConfig) refresh() {
	l.mu.Lock()
	info, err := os.Stat(l.path)
	exists := err == nil
	var modTime time.Time
	var size int64
	if exists {
		modTime, size = info.ModTime(), info.Size()
	}
	if exists == l.exists && modTime.Equal(l.modTime) && size == l.size {
		l.mu.Unlock()
		return
	}
	cfg, loadErr := config.Load(l.path)
	if loadErr != nil {
		l.mu.Unlock()
		log.Printf("config: %v (keeping the settings in use)", loadErr)
		return
	}
	l.cfg, l.modTime, l.size, l.exists = cfg, modTime, size, exists
	onChange := l.onChange
	l.mu.Unlock()
	log.Printf("config: reloaded %s", l.path)
	if onChange != nil {
		onChange(cfg)
	}
}

// reloadingBackend refreshes the configuration before each cast starts.
type reloadingBackend struct {
	daemon.Backend
	refresh func()
}

func (r reloadingBackend) Start(ctx context.Context, device discovery.Device, mode string) error {
	if r.refresh != nil {
		r.refresh()
	}
	return r.Backend.Start(ctx, device, mode)
}
