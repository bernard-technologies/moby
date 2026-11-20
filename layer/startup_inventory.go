package layer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
)

var overlayLayerDirectory = regexp.MustCompile(`^[a-f0-9]{64}(-init)?$`)

// The loaded indexes cannot establish ownership. Failed restores and BuildKit
// snapshots, among others, can be absent from these indexes.
func (ls *layerStore) reportInterruptedLayers(scanner interface{ LayerDirectories() ([]string, error) }, incomplete bool) {
	entries, err := os.ReadDir(filepath.Join(ls.store.root, "tmp"))
	if err != nil && !os.IsNotExist(err) {
		incomplete = true
		logrus.WithError(err).Warn("Could not enumerate staged layer transactions")
	}
	const detailLimit = 10
	staged, unknown := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "write-set-") {
			continue
		}
		staged++
		if staged <= detailLimit {
			logrus.WithField("transaction", entry.Name()).Warn("Staged layer transaction left untouched")
		}
	}

	loaded := make(map[string]struct{}, len(ls.layerMap)+2*len(ls.mounts))
	for _, layer := range ls.layerMap {
		loaded[layer.cacheID] = struct{}{}
	}
	for _, mount := range ls.mounts {
		loaded[mount.mountID] = struct{}{}
		loaded[mount.initID] = struct{}{}
	}
	ids, err := scanner.LayerDirectories()
	if err != nil {
		incomplete = true
		logrus.WithError(err).Warn("Could not enumerate overlay2 layer directories")
	}
	for _, id := range ids {
		if _, ok := loaded[id]; ok || !overlayLayerDirectory.MatchString(id) {
			continue
		}
		unknown++
		if unknown <= detailLimit {
			logrus.WithField("cache-id", id).Debug("Overlay2 directory not matched to loaded layer metadata; ownership unknown")
		}
	}
	if incomplete || unknown != 0 || staged != 0 {
		logrus.WithFields(logrus.Fields{
			"incomplete": incomplete, "staged": staged, "unmatched": unknown,
		}).Info("Layer startup inventory (report only)")
	}
}
