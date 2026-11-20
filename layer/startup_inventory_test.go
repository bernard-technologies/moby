package layer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

type inventoryDirs []string

func (d inventoryDirs) LayerDirectories() ([]string, error) { return d, nil }

func TestStartupInventoryDoesNotRemoveUnknownData(t *testing.T) {
	root := t.TempDir()
	loaded, buildkit, failedMount := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	paths := []string{
		filepath.Join("layerdb", "tmp", "write-set-existing", "cache-id"),
		filepath.Join("layerdb", "tmp", "write-set-malformed", "partial"),
		filepath.Join("overlay2", buildkit, "diff", "snapshot"),
		filepath.Join("overlay2", failedMount, "diff", "container-data"),
		filepath.Join("overlay2", "backingFsBlockDev"),
	}
	for i, path := range paths {
		content := "sentinel"
		if i == 0 {
			content = loaded
		}
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ls := &layerStore{store: &fileMetadataStore{root: filepath.Join(root, "layerdb")},
		layerMap: map[ChainID]*roLayer{"test": {cacheID: loaded}}}
	var logs bytes.Buffer
	logger := logrus.StandardLogger()
	out := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(out)
	ls.reportInterruptedLayers(inventoryDirs{loaded, buildkit, failedMount, "backingFsBlockDev", "l"}, true)
	for _, field := range []string{"staged=2", "unmatched=2", "incomplete=true"} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing %s in %s", field, logs.String())
		}
	}
	for i, path := range paths {
		want := "sentinel"
		if i == 0 {
			want = loaded
		}
		if data, err := os.ReadFile(filepath.Join(root, path)); err != nil || string(data) != want {
			t.Errorf("%s: got %q, %v", path, data, err)
		}
	}
}
