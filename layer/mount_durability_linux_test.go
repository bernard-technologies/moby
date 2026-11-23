package layer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/ioutils"
)

type mountTestDriver struct {
	*durableTestDriver
	rwCreated []string
	onRW      func(id string) error
}

func (d *mountTestDriver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	if d.onRW != nil {
		if err := d.onRW(id); err != nil {
			return err
		}
	}
	if err := d.durableTestDriver.Driver.CreateReadWrite(id, parent, opts); err != nil {
		return err
	}
	d.rwCreated = append(d.rwCreated, id)
	return nil
}

func newMountTestStore(t *testing.T) (*layerStore, *mountTestDriver, Layer) {
	t.Helper()
	_, base := newDurableTestStore(t)
	driver := &mountTestDriver{durableTestDriver: base}
	s, err := newStoreFromGraphDriver(filepath.Join(base.root, "layerdb"), driver, "linux")
	if err != nil {
		t.Fatal(err)
	}
	ls := s.(*layerStore)
	tar, err := tarFromFiles(newTestFile("/base", []byte("base"), 0644))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ls.Register(bytes.NewReader(tar), "")
	if err != nil {
		t.Fatal(err)
	}
	return ls, driver, parent
}

func stagedMounts(t *testing.T, ls *layerStore) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(ls.store.root, "tmp", "write-set-*", "mount-name"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func initWrites(content string) *CreateRWLayerOpts {
	return &CreateRWLayerOpts{InitFunc: func(root containerfs.ContainerFS) error {
		return os.WriteFile(filepath.Join(root.Path(), "init"), []byte(content), 0644)
	}}
}

func TestDurableCreateRWLayerOrder(t *testing.T) {
	ls, driver, parent := newMountTestStore(t)
	mounts := filepath.Join(ls.store.root, "mounts")
	var barrier bool
	driver.onRW = func(id string) error {
		staged := stagedMounts(t, ls)
		if len(staged) != 1 {
			t.Fatalf("no intent before creating %s: %v", id, staged)
		}
		dir := filepath.Dir(staged[0])
		mountID, err := os.ReadFile(filepath.Join(dir, "mount-id"))
		if err != nil {
			t.Fatal(err)
		}
		if id != string(mountID) && id != string(mountID)+"-init" {
			t.Fatalf("creating %s not named by intent %s", id, mountID)
		}
		if _, err := os.Stat(filepath.Join(dir, "init-id")); err != nil {
			t.Fatalf("init-id missing from intent: %v", err)
		}
		return nil
	}
	ls.syncFS = func(f *os.File) error {
		barrier = true
		if len(driver.rwCreated) != 2 {
			t.Fatalf("payload barrier before both layers: %v", driver.rwCreated)
		}
		if _, err := os.Stat(mounts); !os.IsNotExist(err) {
			t.Fatalf("mount record visible before payload barrier: %v", err)
		}
		return ioutils.SyncFilesystem(f)
	}
	rw, err := ls.CreateRWLayer("ctr", parent.ChainID(), initWrites("init"))
	if err != nil {
		t.Fatal(err)
	}
	if !barrier {
		t.Fatal("payload barrier skipped")
	}
	if len(stagedMounts(t, ls)) != 0 {
		t.Fatal("staged record left after success")
	}
	if _, err := os.Stat(filepath.Join(mounts, "ctr", "mount-name")); err != nil {
		t.Fatalf("mount-name not committed with the record: %v", err)
	}
	reopened, err := newStoreFromGraphDriver(ls.store.root, driver, "linux")
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetRWLayer("ctr")
	if err != nil {
		t.Fatal(err)
	}
	m := got.(*referencedRWLayer).mountedLayer
	want := rw.(*referencedRWLayer).mountedLayer
	if m.mountID != want.mountID || m.initID != want.mountID+"-init" || m.parent.chainID != parent.ChainID() {
		t.Fatalf("reopened mount=%+v, want mount-id %s", m, want.mountID)
	}
	fs, err := got.Mount("")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Unmount()
	if data, err := os.ReadFile(filepath.Join(fs.Path(), "init")); err != nil || string(data) != "init" {
		t.Fatalf("init content=%q, err=%v", data, err)
	}
}

func TestDurableCreateRWLayerFailures(t *testing.T) {
	for _, fault := range []string{"intent", "init-create", "rw-create", "payload", "staged"} {
		t.Run(fault, func(t *testing.T) {
			ls, driver, parent := newMountTestStore(t)
			tmp := filepath.Join(ls.store.root, "tmp")
			var payloadDone bool
			driver.onRW = func(id string) error {
				init := strings.HasSuffix(id, "-init")
				if (fault == "init-create" && init) || (fault == "rw-create" && !init) {
					return syscall.ENOSPC
				}
				return nil
			}
			ls.syncDir = func(path string) error {
				if fault == "intent" && path == tmp {
					return syscall.ENOSPC
				}
				if fault == "staged" && payloadDone && filepath.Dir(path) == tmp {
					return syscall.EIO
				}
				return ioutils.SyncDir(path)
			}
			ls.syncFS = func(f *os.File) error {
				payloadDone = true
				if fault == "payload" {
					return syscall.EIO
				}
				return ioutils.SyncFilesystem(f)
			}
			before := parent.(*referencedCacheLayer).roLayer.referenceCount
			_, err := ls.CreateRWLayer("ctr", parent.ChainID(), initWrites("init"))
			if !errors.Is(err, syscall.ENOSPC) && !errors.Is(err, syscall.EIO) {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(filepath.Join(ls.store.root, "mounts", "ctr")); !os.IsNotExist(err) {
				t.Fatalf("partial mount record published: %v", err)
			}
			if after := parent.(*referencedCacheLayer).roLayer.referenceCount; after != before {
				t.Fatalf("parent reference leaked: %d -> %d", before, after)
			}
			switch fault {
			case "init-create", "rw-create":
				// The driver may have left data under a planned ID.
				if len(stagedMounts(t, ls)) != 1 {
					t.Fatal("intent not retained after a failed driver create")
				}
				for _, id := range driver.removed {
					t.Fatalf("removed %s after an uncertain create", id)
				}
			default:
				if len(stagedMounts(t, ls)) != 0 {
					t.Fatal("intent left after full operation-local cleanup")
				}
				if len(driver.removed) != len(driver.rwCreated) {
					t.Fatalf("created=%v removed=%v", driver.rwCreated, driver.removed)
				}
			}
			driver.onRW = nil
			ls.syncDir, ls.syncFS = ioutils.SyncDir, ioutils.SyncFilesystem
			if _, err := ls.CreateRWLayer("ctr", parent.ChainID(), initWrites("init")); err != nil {
				// Retrying with fresh IDs must not be blocked by the old stage.
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestDurableCreateRWLayerUncertainCommit(t *testing.T) {
	ls, driver, parent := newMountTestStore(t)
	mounts := filepath.Join(ls.store.root, "mounts")
	ls.syncDir = func(path string) error {
		if path == mounts {
			if _, err := os.Stat(filepath.Join(mounts, "ctr")); err == nil {
				return syscall.EIO
			}
		}
		return ioutils.SyncDir(path)
	}
	before := parent.(*referencedCacheLayer).roLayer.referenceCount
	if _, err := ls.CreateRWLayer("ctr", parent.ChainID(), initWrites("init")); !errors.Is(err, syscall.EIO) {
		t.Fatalf("expected post-rename EIO, got %v", err)
	}
	if len(driver.removed) != 0 {
		t.Fatalf("removed layers of a published record: %v", driver.removed)
	}
	if after := parent.(*referencedCacheLayer).roLayer.referenceCount; after != before+1 {
		t.Fatalf("parent not pinned: %d -> %d", before, after)
	}
	ls.syncDir = ioutils.SyncDir
	if _, err := ls.GetRWLayer("ctr"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("uncertain mount readable: %v", err)
	}
	if _, err := ls.CreateRWLayer("ctr", parent.ChainID(), nil); !errors.Is(err, syscall.EIO) {
		t.Fatalf("retry acknowledged an uncertain mount: %v", err)
	}
	reopened, err := newStoreFromGraphDriver(ls.store.root, driver, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetRWLayer("ctr"); err != nil {
		t.Fatalf("intact record not reloaded: %v", err)
	}
}

func TestDurableCreateRWLayerExistingRecord(t *testing.T) {
	ls, driver, parent := newMountTestStore(t)
	dir := filepath.Join(ls.store.root, "mounts", "ctr")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.CreateRWLayer("ctr", parent.ChainID(), nil); err == nil {
		t.Fatal("overwrote an existing mount record")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("existing record changed: %q, %v", data, err)
	}
	if len(driver.removed) != len(driver.rwCreated) {
		t.Fatalf("private layers not removed: created=%v removed=%v", driver.rwCreated, driver.removed)
	}
}
