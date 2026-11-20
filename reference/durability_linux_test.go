package reference

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/pkg/ioutils"
	digest "github.com/opencontainers/go-digest"
)

func TestDurableReferenceUpdate(t *testing.T) {
	for _, rename := range []bool{false, true} {
		name := "before-rename"
		if rename {
			name = "after-rename"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repositories.json")
			storeIface, err := NewDurableReferenceStore(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			s := storeIface.(*store)
			ref, err := reference.ParseNormalizedNamed("test:latest")
			if err != nil {
				t.Fatal(err)
			}
			oldID, newID := digest.FromString("old"), digest.FromString("new")
			if err := s.AddTag(ref, oldID, false); err != nil {
				t.Fatal(err)
			}

			s.durableWrite = func(path string, data []byte, mode os.FileMode) (bool, error) {
				if rename {
					// Put the new record on disk without committing the
					// containing directory, then inject the sync error.
					stage := path + ".test"
					if err := os.WriteFile(stage, data, mode); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(stage, path); err != nil {
						t.Fatal(err)
					}
				}
				return rename, syscall.EIO
			}
			if err := s.AddTag(ref, newID, true); !errors.Is(err, syscall.EIO) {
				t.Fatalf("unexpected write result: %v", err)
			}
			if len(s.References(oldID)) != 1 || len(s.References(newID)) != 0 {
				t.Fatal("unconfirmed reference escaped in-memory indexes")
			}
			if rename {
				if _, err := s.Get(ref); !errors.Is(err, syscall.EIO) {
					t.Fatalf("lookup bypassed pending sync: %v", err)
				}
				if err := s.AddTag(ref, newID, true); !errors.Is(err, syscall.EIO) {
					t.Fatalf("retry bypassed pending sync: %v", err)
				}
				for _, id := range []digest.Digest{oldID, newID} {
					if err := WithUnreferencedImage(s, id, func() error {
						t.Fatal("removed image while disk state is uncertain")
						return nil
					}); err == nil {
						t.Fatalf("missing deletion fence for %s", id)
					}
				}
			} else if got, err := s.Get(ref); err != nil || got != oldID {
				t.Fatalf("failed write changed reference: %s, %v", got, err)
			}

			s.durableWrite = ioutils.DurableAtomicWriteFile
			if err := s.AddTag(ref, newID, true); err != nil {
				t.Fatal(err)
			}
			if len(s.References(oldID)) != 0 || len(s.References(newID)) != 1 {
				t.Fatal("indexes not updated after durability was confirmed")
			}
			stock, err := NewReferenceStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := stock.Get(ref); err != nil || got != newID {
				t.Fatalf("stock reader got %s, %v", got, err)
			}
		})
	}
}
