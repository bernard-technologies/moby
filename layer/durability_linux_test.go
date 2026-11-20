package layer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/ioutils"
)

type durableTestDriver struct {
	graphdriver.Driver
	root     string
	created  []string
	removed  []string
	onCreate func(string)
}

func (d *durableTestDriver) OpenLayerWriteBarrier() (*os.File, error) { return os.Open(d.root) }

func (d *durableTestDriver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	d.created = append(d.created, id)
	if d.onCreate != nil {
		d.onCreate(id)
	}
	return d.Driver.Create(id, parent, opts)
}

func (d *durableTestDriver) Remove(id string) error {
	d.removed = append(d.removed, id)
	return d.Driver.Remove(id)
}

func newDurableTestStore(t *testing.T) (*layerStore, *durableTestDriver) {
	t.Helper()
	root := t.TempDir()
	base, err := newVFSGraphDriver(root)
	if err != nil {
		t.Fatal(err)
	}
	driver := &durableTestDriver{Driver: base, root: root}
	store, err := newStoreFromGraphDriver(filepath.Join(root, "layerdb"), driver, "linux")
	if err != nil {
		t.Fatal(err)
	}
	return store.(*layerStore), driver
}

func TestDurableRegisterBeforePublish(t *testing.T) {
	for _, fault := range []string{"intent", "payload", "staged", "none"} {
		t.Run(fault, func(t *testing.T) {
			ls, driver := newDurableTestStore(t)
			tar, err := tarFromFiles(newTestFile("/file", []byte("layer data"), 0644))
			if err != nil {
				t.Fatal(err)
			}
			tmp := filepath.Join(ls.store.root, "tmp")
			var payloadBarrier bool
			driver.onCreate = func(id string) {
				ids, err := filepath.Glob(filepath.Join(tmp, "write-set-*", "cache-id"))
				if err != nil || len(ids) != 1 {
					t.Fatalf("missing intent at create: %v, %v", ids, err)
				}
				if data, err := os.ReadFile(ids[0]); err != nil || string(data) != id {
					t.Fatalf("incorrect cache-id at create: %q, %v", data, err)
				}
			}
			ls.syncFS = func(f *os.File) error {
				payloadBarrier = true
				if _, err := os.Stat(filepath.Join(ls.store.root, "sha256")); !os.IsNotExist(err) {
					t.Fatalf("layerdb materialized before payload barrier: %v", err)
				}
				if fault == "payload" {
					return syscall.EIO
				}
				return ioutils.SyncFilesystem(f)
			}
			ls.syncDir = func(path string) error {
				if fault == "intent" && path == tmp {
					return syscall.ENOSPC
				}
				if fault == "staged" && payloadBarrier && filepath.Dir(path) == tmp {
					return syscall.EIO
				}
				return ioutils.SyncDir(path)
			}
			l, err := ls.Register(bytes.NewReader(tar), "")
			if fault == "none" {
				if err != nil || l == nil || !payloadBarrier || len(driver.removed) != 0 {
					t.Fatalf("register=%v, err=%v, barrier=%v, removed=%v", l, err, payloadBarrier, driver.removed)
				}
				if data, err := ls.store.GetCacheID(l.ChainID()); err != nil || data != driver.created[0] {
					t.Fatalf("committed cache-id=%q, err=%v", data, err)
				}
				return
			}
			want := syscall.EIO
			if fault == "intent" {
				want = syscall.ENOSPC
			}
			if !errors.Is(err, want) || l != nil || len(ls.layerMap) != 0 {
				t.Fatalf("register=%v, err=%v, published=%d", l, err, len(ls.layerMap))
			}
			if fault == "intent" {
				if len(driver.created) != 0 || payloadBarrier {
					t.Fatalf("driver reached despite failed intent: %v", driver.created)
				}
			} else if !payloadBarrier || len(driver.created) != 1 || len(driver.removed) != 1 || driver.created[0] != driver.removed[0] {
				t.Fatalf("operation-local cleanup not performed: create=%v remove=%v", driver.created, driver.removed)
			}
		})
	}
}

func TestPersistedCacheIDIsNotRewritten(t *testing.T) {
	ms, err := newFSMetadataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ms.StartTransaction()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.SetCacheID("test-id"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tx.String(), "cache-id")
	then := time.Unix(1234, 0)
	if err := os.Chtimes(file, then, then); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetCacheID("test-id"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(file); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(then) {
		t.Fatal("later staging rewrote and risked truncating the allocation intent")
	}
	if err := tx.SetCacheID("wrong-id"); err == nil {
		t.Fatal("transaction accepted a different cache ID")
	}
}

func TestDurableRegisterQuarantinesUncertainCommit(t *testing.T) {
	for _, failedParent := range []string{"sha256", "tmp"} {
		t.Run(failedParent, func(t *testing.T) {
			ls, driver := newDurableTestStore(t)
			rootTar, err := tarFromFiles(newTestFile("/base", []byte("base"), 0644))
			if err != nil {
				t.Fatal(err)
			}
			parent, err := ls.Register(bytes.NewReader(rootTar), "")
			if err != nil {
				t.Fatal(err)
			}
			childTar, err := tarFromFiles(newTestFile("/child", []byte("child"), 0644))
			if err != nil {
				t.Fatal(err)
			}
			ls.syncDir = func(path string) error {
				if filepath.Base(path) == failedParent {
					entries, err := os.ReadDir(filepath.Join(ls.store.root, "sha256"))
					if err == nil && len(entries) == 2 {
						return syscall.EIO
					}
				}
				return ioutils.SyncDir(path)
			}
			if _, err := ls.Register(bytes.NewReader(childTar), parent.ChainID()); !errors.Is(err, syscall.EIO) {
				t.Fatalf("expected post-rename EIO, got %v", err)
			}
			var quarantined ChainID
			for id := range ls.uncertain {
				quarantined = id
			}
			if data, err := ls.store.GetCacheID(quarantined); err != nil || data != driver.created[1] || len(driver.removed) != 0 {
				t.Fatalf("renamed record/payload not preserved: cache-id=%q, err=%v, removed=%v", data, err, driver.removed)
			}

			ls.syncDir = ioutils.SyncDir
			if _, err := ls.Get(quarantined); !errors.Is(err, syscall.EIO) {
				t.Fatalf("quarantined layer readable: %v", err)
			}
			if _, err := ls.Register(bytes.NewReader(childTar), parent.ChainID()); !errors.Is(err, syscall.EIO) {
				t.Fatalf("retry acknowledged an uncertain commit: %v", err)
			}
			if _, err := ls.Release(parent); err != nil {
				t.Fatal(err)
			}
			if len(driver.removed) != 1 || driver.removed[0] != driver.created[2] {
				t.Fatalf("expected only the retry's private layer to be removed, got %v", driver.removed)
			}
			reopened, err := newStoreFromGraphDriver(ls.store.root, driver, "linux")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.Get(quarantined); err != nil {
				t.Fatalf("failed to reopen intact record: %v", err)
			}
		})
	}
}
