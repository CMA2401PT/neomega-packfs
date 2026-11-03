package packfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"testing"
)

func TestIncrementalSnapshotIsolation(t *testing.T) {
	p, name := testPack(t)
	large := bytes.Repeat([]byte("m"), 1026*blockSize+71)
	direct := bytes.Repeat([]byte("d"), 8*blockSize)
	deleted := bytes.Repeat([]byte("x"), 20*blockSize)
	must(t, p.WriteFile("overwrite", large))
	must(t, p.WriteFile("truncate", large))
	must(t, p.WriteFile("direct", direct))
	must(t, p.WriteFile("delete", deleted))
	must(t, p.Snapshot("before"))
	before, err := p.SnapshotFS("before")
	must(t, err)
	f, err := p.OpenFile("overwrite", os.O_RDWR)
	must(t, err)
	patch := bytes.Repeat([]byte("p"), blockSize)
	_, err = f.WriteAt(patch, blockSize)
	must(t, err)
	_, err = f.WriteAt([]byte("cross"), 1024*blockSize-2)
	must(t, err)
	must(t, f.Close())
	changed := bytes.Clone(large)
	copy(changed[blockSize:], patch)
	copy(changed[1024*blockSize-2:], "cross")
	f, err = p.OpenFile("truncate", os.O_RDWR)
	must(t, err)
	must(t, f.Truncate(1024*blockSize+3))
	must(t, p.Remove("delete"))
	d, err := p.OpenFile("direct", os.O_RDWR)
	must(t, err)
	must(t, d.Truncate(3*blockSize))
	must(t, d.Close())
	must(t, p.Commit())
	wantFile(t, before, "overwrite", large)
	wantFile(t, before, "truncate", large)
	wantFile(t, before, "direct", direct)
	wantFile(t, before, "delete", deleted)
	must(t, p.Snapshot("middle"))
	middle, err := p.SnapshotFS("middle")
	must(t, err)
	must(t, f.Truncate(5*blockSize+7))
	must(t, p.Commit())
	must(t, f.Truncate(200))
	must(t, p.Commit())
	must(t, f.Truncate(12*blockSize))
	must(t, f.Close())
	must(t, p.WriteFile("overwrite", bytes.Repeat([]byte("z"), len(large))))
	must(t, p.Commit())
	final := make([]byte, 12*blockSize)
	copy(final, large[:200])
	wantFile(t, p, "truncate", final)
	wantFile(t, middle, "overwrite", changed)
	wantFile(t, middle, "truncate", large[:1024*blockSize+3])
	wantFile(t, before, "truncate", large)
	must(t, middle.Close())
	must(t, before.Close())
	must(t, p.Close())
	p, err = Open(name)
	must(t, err)
	defer p.Close()
	wantFile(t, p, "truncate", final)
	wantFile(t, p, "direct", direct[:3*blockSize])
	wantFile(t, p, "overwrite", bytes.Repeat([]byte("z"), len(large)))
	for _, snapshot := range []string{"before", "middle"} {
		v, err := p.SnapshotFS(snapshot)
		must(t, err)
		if snapshot == "before" {
			wantFile(t, v, "overwrite", large)
			wantFile(t, v, "truncate", large)
			wantFile(t, v, "delete", deleted)
		} else {
			wantFile(t, v, "overwrite", changed)
			wantFile(t, v, "truncate", large[:1024*blockSize+3])
		}
		must(t, v.Close())
	}
}

func TestRepeatedCommitsReuseBlocks(t *testing.T) {
	p, name := testPack(t)
	want := bytes.Repeat([]byte("r"), 66*blockSize)
	must(t, p.WriteFile("data", want))
	must(t, p.Commit())
	initial := diskSize(t, name)
	f, err := p.OpenFile("data", os.O_RDWR)
	must(t, err)
	defer f.Close()
	for round := 0; round < 80; round++ {
		off := (round % 66) * blockSize
		data := bytes.Repeat([]byte{byte(round)}, blockSize)
		_, err := f.WriteAt(data, int64(off))
		must(t, err)
		copy(want[off:], data)
		must(t, p.Commit())
		if size := diskSize(t, name); size > initial+8*blockSize {
			t.Fatalf("round %d grew container: initial=%d current=%d", round, initial, size)
		}
	}
	wantFile(t, p, "data", want)
}

func TestUnlinkedHandleRetainsYoungBlocks(t *testing.T) {
	p, name := testPack(t)
	want := bytes.Repeat([]byte("h"), 20*blockSize)
	must(t, p.WriteFile("orphan", want))
	f, err := p.OpenFile("orphan", os.O_RDWR)
	must(t, err)
	reader, err := p.OpenFile("orphan", os.O_RDONLY)
	must(t, err)
	must(t, p.Commit())
	must(t, p.Remove("orphan"))
	must(t, p.Commit())
	must(t, p.WriteFile("other", bytes.Repeat([]byte("o"), len(want))))
	must(t, p.Commit())
	_, err = f.WriteAt([]byte("live"), 11)
	must(t, err)
	copy(want[11:], "live")
	must(t, p.Commit())
	must(t, f.Close())
	must(t, p.Commit())
	got, err := io.ReadAll(reader)
	must(t, err)
	if !bytes.Equal(got, want) {
		t.Fatal("closing writable handle reclaimed an object still open for reading")
	}
	ids := make(map[uint32]bool)
	for i := uint32(0); i < reader.obj.size/blockSize; i++ {
		id, err := p.store.dataID(reader.obj, i)
		must(t, err)
		ids[id] = true
	}
	must(t, reader.Close())
	must(t, p.Commit())
	for id := range ids {
		if p.store.has(id) {
			t.Fatalf("last handle close did not release block %d", id)
		}
	}
	before := diskSize(t, name)
	must(t, p.WriteFile("reused", want))
	must(t, p.Commit())
	if after := diskSize(t, name); after > before+4*blockSize {
		t.Fatalf("orphan space was not reused: %d -> %d", before, after)
	}
	wantFile(t, p, "reused", want)
}

func TestOrdinaryCommitsSkipRebuild(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	must(t, p.WriteFile("data", bytes.Repeat([]byte("b"), 256*blockSize)))
	must(t, p.Snapshot("base"))
	must(t, p.Snapshot("other"))
	root := p.snapshotIndex["base"]
	f, err := p.OpenFile("data", os.O_RDWR)
	must(t, err)
	scans := 0
	for round := 0; round < 32; round++ {
		_, err := f.WriteAt([]byte{byte(round)}, 17)
		must(t, err)
		disk.reads = make(map[uint32]int)
		must(t, p.Commit())
		scans += disk.reads[root]
	}
	if scans != 0 {
		t.Fatalf("ordinary commits performed %d full scans", scans)
	}
	must(t, f.Close())
	disk.reads = make(map[uint32]int)
	must(t, p.DeleteSnapshot("other"))
	if scans := disk.reads[root]; scans != 1 {
		t.Fatalf("DeleteSnapshot scanned the shared root %d times", scans)
	}
	disk.reads = make(map[uint32]int)
	must(t, p.Restore("base"))
	if scans := disk.reads[root]; scans != 1 {
		t.Fatalf("Restore scanned the shared root %d times", scans)
	}
}

func TestOldGarbageTriggersOneScan(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	original := bytes.Repeat([]byte("o"), 80*blockSize)
	changed := bytes.Repeat([]byte("c"), len(original))
	must(t, p.WriteFile("data", original))
	must(t, p.Snapshot("base"))
	root := p.snapshotIndex["base"]
	must(t, p.WriteFile("data", changed))
	disk.reads = make(map[uint32]int)
	must(t, p.Commit())
	if scans := disk.reads[root]; scans != 1 {
		t.Fatalf("old garbage triggered %d scans", scans)
	}
	if p.store.oldGarbage != 0 {
		t.Fatal("full scan did not reset old garbage count")
	}
	wantFile(t, p, "data", changed)
	v, err := p.SnapshotFS("base")
	must(t, err)
	wantFile(t, v, "data", original)
	must(t, v.Close())
}

func TestUnchangedCommitDoesNoIO(t *testing.T) {
	disk := &crashDisk{}
	p, err := openStore(disk, 0)
	must(t, err)
	must(t, p.WriteFile("file", []byte("same")))
	must(t, p.Commit())
	writes, syncs, truncates, generation := disk.writes, disk.syncs, disk.truncates, p.generation
	for i := 0; i < 5; i++ {
		must(t, p.Commit())
	}
	f, err := p.OpenFile("file", os.O_RDONLY)
	must(t, err)
	must(t, f.Sync())
	must(t, f.Close())
	must(t, p.Close())
	if disk.writes != writes || disk.syncs != syncs || disk.truncates != truncates || p.generation != generation {
		t.Fatalf("unchanged commit wrote or synced: writes=%d syncs=%d generation=%d", disk.writes-writes, disk.syncs-syncs, p.generation-generation)
	}
}

// spillData 为每个数据块留下不同标记，读回时可以识别映射串块和内容丢失。
func spillData() []byte {
	data := bytes.Repeat([]byte("0123456789abcdef"), (dirtyLimit+2*1024*1024)/16)
	for off := 0; off < len(data); off += blockSize {
		binary.LittleEndian.PutUint32(data[off:], uint32(off/blockSize))
	}
	return data
}

func TestSpillBoundsAndReopen(t *testing.T) {
	p, name := testPack(t)
	f, err := p.Create("large")
	must(t, err)
	data := spillData()
	generation := p.generation
	initial := diskSize(t, name)
	n, err := f.Write(data)
	must(t, err)
	if n != len(data) {
		t.Fatal("short write")
	}
	if len(p.store.dirty)*blockSize > dirtyLimit {
		t.Fatal("dirty buffers exceeded the limit")
	}
	if diskSize(t, name) <= initial || p.generation != generation {
		t.Fatal("large write did not spill without publishing")
	}
	first, err := p.store.dataID(f.obj, 0)
	must(t, err)
	if p.store.dirty[first] != nil {
		t.Fatal("first data block was not spilled")
	}
	owned := len(p.store.owned)
	_, err = f.WriteAt([]byte("reload-owned"), 37)
	must(t, err)
	copy(data[37:], "reload-owned")
	after, err := p.store.dataID(f.obj, 0)
	must(t, err)
	if first != after || len(p.store.owned) != owned {
		t.Fatal("spilled owned blocks were copied instead of reloaded")
	}
	must(t, p.Commit())
	must(t, f.Close())
	must(t, p.Close())
	p, err = Open(name)
	must(t, err)
	defer p.Close()
	f, err = p.OpenFile("large", os.O_RDONLY)
	must(t, err)
	defer f.Close()
	buf := make([]byte, 1024*1024)
	for off := 0; off < len(data); off += len(buf) {
		_, err := f.ReadAt(buf, int64(off))
		must(t, err)
		if !bytes.Equal(buf, data[off:off+len(buf)]) {
			t.Fatalf("large file differs at offset %d", off)
		}
	}
}

func TestDiscardSpilledTransaction(t *testing.T) {
	data := spillData()
	for _, mode := range []string{"power-cut", "write-failure", "restore"} {
		t.Run(mode, func(t *testing.T) {
			disk := &crashDisk{}
			p, err := openStore(disk, 0)
			must(t, err)
			original := bytes.Repeat([]byte("s"), 3*blockSize)
			must(t, p.WriteFile("data", original))
			must(t, p.Snapshot("base"))
			f, err := p.OpenFile("data", os.O_RDWR)
			must(t, err)
			syncs := disk.syncs
			if mode == "write-failure" {
				disk.failWrite = disk.writes + 2
			}
			_, err = f.WriteAt(data, 0)
			if mode == "write-failure" {
				if err != errPowerLoss {
					t.Fatalf("spill failure was not sticky: %v", err)
				}
				writes := disk.writes
				if err := p.Close(); err != errPowerLoss {
					t.Fatalf("faulted close: %v", err)
				}
				if disk.writes != writes {
					t.Fatal("faulted close retried the spill")
				}
			} else {
				must(t, err)
				if disk.syncs != syncs {
					t.Fatal("spill synchronized the uncommitted transaction")
				}
				if len(p.store.dirty)*blockSize > dirtyLimit {
					t.Fatal("spill did not bound dirty buffers")
				}
				if mode == "restore" {
					must(t, f.Close())
					must(t, p.Restore("base"))
					if len(p.store.owned) != 0 || len(p.store.freed) != 0 {
						t.Fatal("Restore retained discarded transaction state")
					}
					wantFile(t, p, "data", original)
					f, err := p.OpenFile("data", os.O_RDWR)
					must(t, err)
					_, err = f.WriteAt([]byte("cow"), 17)
					must(t, err)
					must(t, f.Close())
					must(t, p.Commit())
					v, err := p.SnapshotFS("base")
					must(t, err)
					wantFile(t, v, "data", original)
					must(t, v.Close())
					return
				}
				disk.cut()
			}
			recoveredDisk := cloneDisk(disk.durable)
			recovered, err := openStore(recoveredDisk, int64(len(recoveredDisk.live)))
			must(t, err)
			wantFile(t, recovered, "data", original)
			v, err := recovered.SnapshotFS("base")
			must(t, err)
			wantFile(t, v, "data", original)
			must(t, v.Close())
		})
	}
}

func TestYoungContentReclaimedAcrossStorageTiers(t *testing.T) {
	for _, size := range []int{3*blockSize + 17, 1026*blockSize + 37} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			p, name := testPack(t)
			f, err := p.Create("data")
			must(t, err)
			defer f.Close()
			for round := 0; round < 6; round++ {
				_, err := f.WriteAt(bytes.Repeat([]byte("x"), size), 0)
				must(t, err)
				must(t, p.Commit())
				must(t, f.Truncate(200))
				must(t, p.Commit())
				limit := int64((size+blockSize-1)/blockSize+12) * blockSize
				if end := diskSize(t, name); end > limit {
					t.Fatalf("round %d retained discarded mappings: size=%d", round, end)
				}
			}
		})
	}
}
