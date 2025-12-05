/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/log"
	fsconfig "github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/fsnotify/fsnotify"
	"github.com/pelletier/go-toml"
)

// hotReloadBlacklist contains the list of configuration fields that do not support hot reloading.
var hotReloadBlacklist = []string{
	"NoPrometheus",
}

// WatchConfig monitors the specified configuration file for changes.
// It triggers the config reload when a change is detected.
func WatchConfig(
	ctx context.Context,
	filePath string,
	rs snapshots.Snapshotter,
	initialConfig *fsconfig.Config,
) error {
	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}

	watchDir := filepath.Dir(absFilePath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := watcher.Add(watchDir); err != nil {
		watcher.Close()
		return err
	}

	log.G(ctx).Infof("started monitoring config file: %s", absFilePath)

	cw := &configWatcher{
		lastConfig: initialConfig,
	}

	go func() {
		defer watcher.Close()

		var (
			debounceTimer *time.Timer
			mu            sync.Mutex
		)

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if event.Name == absFilePath {
					// Trigger on Write, Create, Rename, or Chmod events
					// such as vim, nano, etc.
					if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) ||
						event.Has(fsnotify.Rename) || event.Has(fsnotify.Chmod) {

						mu.Lock()
						if debounceTimer != nil {
							debounceTimer.Stop()
						}
						// Debounce changes with a 50ms delay
						debounceTimer = time.AfterFunc(50*time.Millisecond, func() {
							log.G(ctx).Infof("config file modification detected: %s", absFilePath)
							cw.reload(ctx, absFilePath, rs)
						})
						mu.Unlock()
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.G(ctx).WithError(err).Error("config watcher encountered an error")
			}
		}
	}()

	return nil
}

type configWatcher struct {
	lastConfig *fsconfig.Config
	mu         sync.Mutex
}

func (w *configWatcher) reload(ctx context.Context, configPath string, rs snapshots.Snapshotter) {
	log.G(ctx).Infof("Config file %s changed, reloading...", configPath)
	var newConfig snapshotterConfig
	tree, err := toml.LoadFile(configPath)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to reload config file")
		return
	}
	if err := tree.Unmarshal(&newConfig); err != nil {
		log.G(ctx).WithError(err).Error("failed to unmarshal config")
		return
	}

	newFsConfig := newConfig.Config.Config

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.lastConfig != nil {
		revertBlacklistedChanges(ctx, w.lastConfig, &newFsConfig)
	}

	if w.lastConfig != nil && reflect.DeepEqual(*w.lastConfig, newFsConfig) {
		log.G(ctx).Info("Config content unchanged, skipping update")
		return
	}

	if updater, ok := rs.(interface {
		UpdateConfig(context.Context, fsconfig.Config) error
	}); ok {
		log.G(ctx).Debugf("applying new config: %+v", newFsConfig)
		if err := updater.UpdateConfig(ctx, newFsConfig); err != nil {
			log.G(ctx).WithError(err).Error("failed to update config")
		} else {
			log.G(ctx).Info("Config updated successfully")
			cfgCopy := newFsConfig
			w.lastConfig = &cfgCopy
		}
	} else {
		log.G(ctx).Warn("snapshotter does not support config update")
	}
}

func revertBlacklistedChanges(ctx context.Context, oldConfig, newConfig *fsconfig.Config) {
	oldVal := reflect.ValueOf(oldConfig).Elem()
	newVal := reflect.ValueOf(newConfig).Elem()

	for _, fieldName := range hotReloadBlacklist {
		fOld := oldVal.FieldByName(fieldName)
		fNew := newVal.FieldByName(fieldName)

		if !fOld.IsValid() || !fNew.IsValid() {
			log.G(ctx).Warnf("Field %s not found in config struct", fieldName)
			continue
		}

		if !reflect.DeepEqual(fOld.Interface(), fNew.Interface()) {
			log.G(ctx).Warnf("Ignoring update for '%s' as it does not support hot reloading. Keeping old value: %v",
				fieldName, fOld.Interface())
			fNew.Set(fOld)
		}
	}
}
