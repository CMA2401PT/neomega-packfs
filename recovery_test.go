package packfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"testing"
)

var errPowerLoss = errors.New("simulated power loss")

// crashDisk 只让 Sync 后的字节存活，第 N 次写或同步可触发掉电。
type crashDisk struct {
	live         []byte
	durable      []byte
	writes       int
	syncs        int
	truncates    int
	failWrite    int
	failSync     int
	failTruncate bool
	dead         bool
	reads        map[uint32]int
}

func (d *crashDisk) ReadAt(b []byte, off int64) (int, error) {
	if d.reads != nil {
		d.reads[uint32(off/blockSize)]++
	}
	if off >= int64(len(d.live)) {
		return 0, io.EOF
	}
	n := copy(b, d.live[int(off):])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (d *crashDisk) cut() error {
	d.live = bytes.Clone(d.durable)
	d.dead = true
	return errPowerLoss
}

func (d *crashDisk) WriteAt(b []byte, off int64) (int, error) {
	if d.dead {
		return 0, errPowerLoss
	}
	d.writes++
	end := int(off) + len(b)
	if end > len(d.live) {
		d.live = append(d.live, make([]byte, end-len(d.live))...)
	}
	copy(d.live[int(off):], b)
	if d.writes == d.failWrite {
		return len(b), d.cut()
	}
	return len(b), nil
}

func (d *crashDisk) Truncate(size int64) error {
	d.truncates++
	if d.dead {
		return errPowerLoss
	}
	if d.failTruncate {
		return d.cut()
	}
	if int(size) > len(d.live) {
		d.live = append(d.live, make([]byte, int(size)-len(d.live))...)
	} else {
		d.live = d.live[:int(size)]
	}
	return nil
}

func (d *crashDisk) Sync() error {
	if d.dead {
		return errPowerLoss
	}
	d.syncs++
	if d.syncs == d.failSync {
		return d.cut()
	}
	d.durable = bytes.Clone(d.live)
	return nil
}

func cloneDisk(b []byte) *crashDisk { return &crashDisk{live: bytes.Clone(b), durable: bytes.Clone(b)} }

type savedState struct {
	current   map[string]string
	snapshots map[string]map[string]string
}

func captureTree(t *testing.T, filesystem fs.FS) map[string]string {
	t.Helper()
	state := make(map[string]string)
	must(t, fs.WalkDir(filesystem, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[name+"/"] = ""
			return nil
		}
		b, err := fs.ReadFile(filesystem, name)
		state[name] = string(b)
		return err
	}))
	return state
}

func captureState(t *testing.T, p *PackFS) savedState {
	t.Helper()
	state := savedState{current: captureTree(t, p), snapshots: make(map[string]map[string]string)}
	for _, name := range p.Snapshots() {
		v, err := p.SnapshotFS(name)
		must(t, err)
		state.snapshots[name] = captureTree(t, v)
		must(t, v.Close())
	}
	return state
}

// crashWorkload 让后一次提交复用前一次释放的块，并覆盖快照删除与恢复的发布点。
func crashWorkload(t *testing.T, p *PackFS, checkpoint func()) error {
	t.Helper()
	f, err := p.OpenFile("world/data", os.O_RDWR)
	if err != nil {
		return err
	}
	_, err = f.WriteAt([]byte("partial block overwrite"), 4090)
	if err != nil {
		return err
	}
	must(t, f.Close())
	if err := p.RemoveAll("world/remove"); err != nil {
		return err
	}
	if err := p.WriteFile("new", bytes.Repeat([]byte("n"), 12*blockSize)); err != nil {
		return err
	}
	if err := p.Commit(); err != nil {
		return err
	}
	checkpoint()
	if err := p.DeleteSnapshot("base"); err != nil {
		return err
	}
	checkpoint()
	if err := p.WriteFile("world/data", bytes.Repeat([]byte("replacement"), blockSize)); err != nil {
		return err
	}
	if err := p.Remove("new"); err != nil {
		return err
	}
	if err := p.Commit(); err != nil {
		return err
	}
	checkpoint()
	if err := p.Snapshot("newer"); err != nil {
		return err
	}
	checkpoint()
	if err := p.WriteFile("uncommitted", []byte("discard")); err != nil {
		return err
	}
	if err := p.Restore("original"); err != nil {
		return err
	}
	checkpoint()
	return nil
}

func TestCrashEveryWriteAndSync(t *testing.T) {
	seed := &crashDisk{}
	p, err := openStore(seed, 0)
	must(t, err)
	must(t, p.MkdirAll("world/remove/deep"))
	must(t, p.WriteFile("world/remove/deep/data", bytes.Repeat([]byte("delete"), 20*blockSize)))
	must(t, p.WriteFile("world/data", bytes.Repeat([]byte("old"), 5*blockSize)))
	must(t, p.Snapshot("original"))
	must(t, p.Snapshot("base"))
	baseline := bytes.Clone(seed.durable)
	probe := cloneDisk(baseline)
	probeFS, err := openStore(probe, int64(len(probe.live)))
	must(t, err)
	must(t, crashWorkload(t, probeFS, func() {}))
	for _, mode := range []struct {
		name  string
		count int
	}{{"write", probe.writes}, {"sync", probe.syncs}} {
		for n := 1; n <= mode.count; n++ {
			t.Run(fmt.Sprintf("%s/%d", mode.name, n), func(t *testing.T) {
				disk := cloneDisk(baseline)
				if mode.name == "write" {
					disk.failWrite = n
				} else {
					disk.failSync = n
				}
				p, err := openStore(disk, int64(len(disk.live)))
				must(t, err)
				last := captureState(t, p)
				err = crashWorkload(t, p, func() { last = captureState(t, p) })
				if err != errPowerLoss {
					t.Fatalf("fault did not propagate: %v", err)
				}
				writes, syncs := disk.writes, disk.syncs
				if err := p.Commit(); err != errPowerLoss {
					t.Fatalf("commit after fault: %v", err)
				}
				if err := p.Mkdir("later"); err != errPowerLoss {
					t.Fatalf("mutation after fault: %v", err)
				}
				if err := p.Close(); err != errPowerLoss {
					t.Fatalf("close after fault: %v", err)
				}
				if disk.writes != writes || disk.syncs != syncs {
					t.Fatal("faulted close retried commit")
				}
				reopenedDisk := cloneDisk(disk.durable)
				reopened, err := openStore(reopenedDisk, int64(len(reopenedDisk.live)))
				must(t, err)
				if got := captureState(t, reopened); !reflect.DeepEqual(got, last) {
					t.Fatal("reopened content differs from last successful commit")
				}
			})
		}
	}
}

func TestFaultIsSticky(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	f, err := p.Create("file")
	must(t, err)
	_, err = f.Write([]byte("data"))
	must(t, err)
	disk.failWrite = disk.writes + 1
	if err := f.Sync(); err != errPowerLoss {
		t.Fatalf("sync fault: %v", err)
	}
	for _, operation := range []func() error{
		func() error { return p.WriteFile("new", nil) },
		func() error { _, err := p.Create("new"); return err },
		func() error { return p.Mkdir("new") },
		func() error { return p.MkdirAll("new/dir") },
		func() error { return p.Remove("file") },
		func() error { return p.RemoveAll("file") },
		func() error { return p.Rename("file", "new") },
		func() error { return p.Snapshot("snapshot") },
		func() error { return p.DeleteSnapshot("snapshot") },
		func() error { return p.Restore("snapshot") },
		func() error { return p.Compact() },
		func() error { _, err := f.Write([]byte("x")); return err },
		func() error { _, err := f.WriteAt([]byte("x"), 0); return err },
		func() error { return f.Truncate(0) },
		f.Sync,
		p.Commit,
		p.Close,
	} {
		if err := operation(); err != errPowerLoss {
			t.Fatalf("lost original error: %v", err)
		}
	}
}

func TestTruncateFailureAfterPublication(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	must(t, p.WriteFile("file", []byte("published")))
	must(t, p.WriteFile("temporary", bytes.Repeat([]byte("x"), 20*blockSize)))
	must(t, p.Remove("temporary"))
	disk.failTruncate = true
	if err := p.Commit(); err != errPowerLoss {
		t.Fatalf("truncate fault: %v", err)
	}
	if err := p.Close(); err != errPowerLoss {
		t.Fatalf("close fault: %v", err)
	}
	recoveredDisk := cloneDisk(disk.durable)
	recovered, err := openStore(recoveredDisk, int64(len(recoveredDisk.live)))
	must(t, err)
	wantFile(t, recovered, "file", []byte("published"))
}

func TestSuperblockRecovery(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	must(t, p.WriteFile("file", []byte("before")))
	must(t, p.Commit())
	must(t, p.WriteFile("file", []byte("after")))
	disk.failWrite = disk.writes + len(p.store.dirty) + 1
	if err := p.Commit(); err != errPowerLoss {
		t.Fatalf("superblock cut: %v", err)
	}
	b := bytes.Clone(disk.durable)
	newSlot := (p.generation + 1) % 2
	copy(b[int(newSlot)*blockSize:], []byte("torn"))
	recoveredDisk := cloneDisk(b)
	recovered, err := openStore(recoveredDisk, int64(len(b)))
	must(t, err)
	wantFile(t, recovered, "file", []byte("before"))
	b[0] ^= 0xff
	b[blockSize] ^= 0xff
	broken := cloneDisk(b)
	if _, err := openStore(broken, int64(len(b))); err == nil {
		t.Fatal("opened two invalid superblocks")
	}
	for _, size := range []int{1, blockSize - 1, 2 * blockSize} {
		broken := cloneDisk(make([]byte, size))
		if _, err := openStore(broken, int64(size)); err == nil {
			t.Fatalf("initialized existing nonempty file of length %d", size)
		}
	}
}

func TestOpenScansOnlyMetadataAndDeduplicatesSharing(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	must(t, p.WriteFile("mapped", bytes.Repeat([]byte("m"), 1100*blockSize)))
	must(t, p.WriteFile("direct", bytes.Repeat([]byte("d"), 4*blockSize)))
	must(t, p.Snapshot("one"))
	must(t, p.Snapshot("two"))
	must(t, p.Snapshot("three"))
	dataIDs := make(map[uint32]bool)
	for _, name := range []string{"mapped", "direct"} {
		o, err := p.resolve(p.root, name, true)
		must(t, err)
		for i := uint32(0); i < o.size/blockSize; i++ {
			id, err := p.store.dataID(o, i)
			must(t, err)
			dataIDs[id] = true
		}
	}
	disk.reads = make(map[uint32]int)
	_, err = openStore(disk, int64(len(disk.live)))
	must(t, err)
	for id := range dataIDs {
		if disk.reads[id] != 0 {
			t.Fatalf("opening read data block %d", id)
		}
	}
	rootReads := disk.reads[p.root]
	if rootReads != 1 {
		t.Fatalf("shared namespace root scanned %d times", rootReads)
	}
}
