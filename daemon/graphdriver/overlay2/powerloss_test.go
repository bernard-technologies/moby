//go:build linux
// +build linux

package overlay2 // import "github.com/docker/docker/daemon/graphdriver/overlay2"

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCreateMetadataPowerLoss simulates a power cut after layer creation and
// checks that the overlay2 layer metadata (link, lower, l/ symlink) is either
// absent or complete, never empty.
//
// The scenario mirrors what ext4 (data=ordered, delalloc) does on a real power
// cut: the journal has committed the metadata (new directories, files,
// symlinks), but file contents that were never flushed are lost, leaving
// zero-length files. Plain os.WriteFile without fsync produces exactly that;
// an empty "lower" later makes container creation fail with
// "too many levels of symbolic links".
//
// Mechanism:
//  1. ext4 on a dm-flakey device backed by a loop file.
//  2. Create a base layer and sync it.
//  3. Create child layers, then fsync the directories only. This forces a
//     journal commit of the metadata without writing back file data.
//  4. Switch dm-flakey to drop_writes: every later write is silently lost
//     (the "power cut"). Unmount, restore the device, remount.
//  5. Check every child layer's metadata and mount it.
//
// Requires root, losetup, mkfs.ext4, blockdev, dmsetup and the dm-flakey
// kernel module. Opt in with DOCKER_TEST_POWERLOSS=1.
func TestCreateMetadataPowerLoss(t *testing.T) {
	if os.Getenv("DOCKER_TEST_POWERLOSS") == "" {
		t.Skip("set DOCKER_TEST_POWERLOSS=1 (needs root, loop devices, dm-flakey)")
	}
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}
	for _, tool := range []string{"losetup", "mkfs.ext4", "blockdev", "dmsetup"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}

	const children = 20
	base := t.TempDir()
	img := filepath.Join(base, "disk.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	loop := run(t, "losetup", "--find", "--show", img)
	t.Cleanup(func() { exec.Command("losetup", "-d", loop).Run() })
	run(t, "mkfs.ext4", "-q", "-F", loop)
	sectors := run(t, "blockdev", "--getsz", loop)

	name := fmt.Sprintf("overlay2-powerloss-%d", os.Getpid())
	dev := "/dev/mapper/" + name
	table := func(mode string) string { return fmt.Sprintf("0 %s flakey %s 0 %s", sectors, loop, mode) }
	const up = "180 0"                       // all I/O passes
	const dropWrites = "0 180 1 drop_writes" // writes silently discarded
	run(t, "dmsetup", "create", name, "--table", table(up))
	t.Cleanup(func() { exec.Command("dmsetup", "remove", "--force", name).Run() })
	setTable := func(mode string, nolockfs bool) {
		args := []string{"suspend"}
		if nolockfs {
			args = append(args, "--nolockfs") // do not flush the filesystem first
		}
		run(t, "dmsetup", append(args, name)...)
		run(t, "dmsetup", "load", name, "--table", table(mode))
		run(t, "dmsetup", "resume", name)
	}

	mnt := filepath.Join(base, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	mountFS := func() {
		if err := unix.Mount(dev, mnt, "ext4", 0, ""); err != nil {
			t.Fatalf("mount: %v", err)
		}
	}
	mountFS()
	t.Cleanup(func() { unix.Unmount(mnt, unix.MNT_DETACH) })

	home := filepath.Join(mnt, "overlay2")
	drv, err := Init(home, nil, nil, nil)
	if err != nil {
		t.Skipf("overlay2 not supported here: %v", err)
	}
	if err := drv.Create("base", "", nil); err != nil {
		t.Fatal(err)
	}
	syncFS(t, home)

	ids := make([]string, children)
	for i := range ids {
		ids[i] = fmt.Sprintf("child%02d", i)
		if err := drv.Create(ids[i], "base", nil); err != nil {
			t.Fatal(err)
		}
	}

	// Commit metadata to the journal without writing back file data.
	fsyncDir(t, home)
	fsyncDir(t, filepath.Join(home, linkDir))
	fsyncDir(t, filepath.Join(home, "base"))
	for _, id := range ids {
		fsyncDir(t, filepath.Join(home, id))
	}

	// Power cut: lose everything not yet on disk.
	setTable(dropWrites, true)
	drv.Cleanup()
	if err := unix.Unmount(mnt, 0); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	// Invalidate the block device's buffer cache. Otherwise the remount can
	// read cached blocks whose writes were dropped, mixing pre- and post-cut
	// state. That corrupts the filesystem itself, which a real power cut does
	// not do, and would be a harness failure rather than a code failure.
	run(t, "blockdev", "--flushbufs", dev)
	setTable(up, false)
	run(t, "blockdev", "--flushbufs", dev)
	mountFS()

	// Harness sanity check: the base layer was synced before the cut and must
	// be fully intact. If it is not, the simulation is broken.
	if p := checkBase(home); len(p) > 0 {
		t.Fatalf("harness error, filesystem inconsistent after simulated cut:\n  %s", strings.Join(p, "\n  "))
	}

	var problems []string
	for _, id := range ids {
		problems = append(problems, checkLayer(home, id)...)
	}

	drv, err = Init(home, nil, nil, nil)
	if err != nil {
		t.Fatalf("re-init after power loss: %v", err)
	}
	defer drv.Cleanup()
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(home, id)); err != nil {
			continue
		}
		if _, err := drv.Get(id, ""); err != nil {
			problems = append(problems, fmt.Sprintf("%s: mount failed: %v", id, err))
			continue
		}
		drv.Put(id)
	}

	if len(problems) > 0 {
		t.Fatalf("%d problem(s) after simulated power loss:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// checkBase verifies the base layer, which was synced before the cut.
func checkBase(home string) []string {
	var p []string
	for _, path := range []string{home, filepath.Join(home, linkDir), filepath.Join(home, "base", diffDirName)} {
		if _, err := os.Stat(path); err != nil {
			p = append(p, err.Error())
		}
	}
	if link, err := os.ReadFile(filepath.Join(home, "base", "link")); err != nil || len(link) == 0 {
		p = append(p, fmt.Sprintf("base link: %q %v", link, err))
	}
	return p
}

// checkLayer verifies the invariants of a child layer's metadata.
func checkLayer(home, id string) []string {
	var p []string
	dir := filepath.Join(home, id)
	if _, err := os.Stat(dir); err != nil {
		return []string{fmt.Sprintf("%s: layer directory lost after fsync: %v", id, err)}
	}
	link, err := os.ReadFile(filepath.Join(dir, "link"))
	switch {
	case err != nil:
		p = append(p, fmt.Sprintf("%s: link: %v", id, err))
	case len(link) == 0:
		p = append(p, fmt.Sprintf("%s: link is EMPTY", id))
	default:
		target, err := os.Readlink(filepath.Join(home, linkDir, string(link)))
		if err != nil {
			p = append(p, fmt.Sprintf("%s: l/%s: %v", id, link, err))
		} else if target != filepath.Join("..", id, diffDirName) {
			p = append(p, fmt.Sprintf("%s: l/%s -> %s", id, link, target))
		}
	}
	lower, err := os.ReadFile(filepath.Join(dir, lowerFile))
	switch {
	case err != nil:
		p = append(p, fmt.Sprintf("%s: lower: %v", id, err))
	case len(lower) == 0:
		p = append(p, fmt.Sprintf("%s: lower is EMPTY", id))
	default:
		for _, e := range strings.Split(string(lower), ":") {
			if _, err := os.Stat(filepath.Join(home, e)); err != nil {
				p = append(p, fmt.Sprintf("%s: lower entry %q: %v", id, e, err))
			}
		}
	}
	return p
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func fsyncDir(t *testing.T, path string) {
	t.Helper()
	d, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		t.Fatalf("fsync %s: %v", path, err)
	}
}

func syncFS(t *testing.T, path string) {
	t.Helper()
	d, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := unix.Syncfs(int(d.Fd())); err != nil {
		t.Fatalf("syncfs: %v", err)
	}
}
