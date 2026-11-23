//go:build linux
// +build linux

package overlay2 // import "github.com/docker/docker/daemon/graphdriver/overlay2"

import (
	"os"
	"path"
	"strings"
	"testing"
)

// writeLayer creates the on-disk layout overlay2.create produces, without mounting.
func writeLayer(t *testing.T, d *Driver, id, lid, lower string) {
	t.Helper()
	dir := d.dir(id)
	if err := os.MkdirAll(path.Join(dir, diffDirName), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path.Join("..", id, diffDirName), path.Join(d.home, linkDir, lid)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(dir, "link"), []byte(lid), 0644); err != nil {
		t.Fatal(err)
	}
	if lower == "" {
		return
	}
	if err := os.Mkdir(path.Join(dir, workDirName), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(dir, lowerFile), []byte(lower), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnoseLayer(t *testing.T) {
	const (
		baseLID  = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
		childLID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	)
	for _, tc := range []struct {
		name, want string
		corrupt    func(d *Driver)
	}{
		{name: "valid"},
		{name: "valid root", want: "", corrupt: func(d *Driver) {}},
		{name: "empty lower", want: "empty lower", corrupt: func(d *Driver) {
			os.WriteFile(path.Join(d.dir("child"), lowerFile), nil, 0644)
		}},
		{name: "missing lower on child", want: "missing lower", corrupt: func(d *Driver) {
			os.Remove(path.Join(d.dir("child"), lowerFile))
		}},
		{name: "empty link", want: "invalid link file", corrupt: func(d *Driver) {
			os.WriteFile(path.Join(d.dir("child"), "link"), nil, 0644)
		}},
		{name: "missing lower symlink", want: "lower entry 0", corrupt: func(d *Driver) {
			os.Remove(path.Join(d.home, linkDir, baseLID))
		}},
		{name: "self-referencing link", want: "unexpected target", corrupt: func(d *Driver) {
			p := path.Join(d.home, linkDir, baseLID)
			os.Remove(p)
			os.Symlink(baseLID, p)
		}},
		{name: "wrong own symlink", want: "points to", corrupt: func(d *Driver) {
			p := path.Join(d.home, linkDir, childLID)
			os.Remove(p)
			os.Symlink("../base/diff", p)
		}},
		{name: "garbage lower", want: "invalid lower entry", corrupt: func(d *Driver) {
			os.WriteFile(path.Join(d.dir("child"), lowerFile), []byte("l/x:../../etc"), 0644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Driver{home: t.TempDir()}
			if err := os.Mkdir(path.Join(d.home, linkDir), 0700); err != nil {
				t.Fatal(err)
			}
			writeLayer(t, d, "base", baseLID, "")
			writeLayer(t, d, "child", childLID, path.Join(linkDir, baseLID))
			id := "child"
			if tc.name == "valid root" {
				id = "base"
			}
			if tc.corrupt != nil {
				tc.corrupt(d)
			}
			err := d.diagnoseLayer(id)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected diagnosis: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "overlay2 layer") {
				t.Fatalf("diagnosis %v, want %q", err, tc.want)
			}
		})
	}
}
