package packfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func testPack(t *testing.T) (*PackFS, string) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "world.pack")
	p, err := Open(name)
	must(t, err)
	t.Cleanup(func() {
		if !p.closed {
			must(t, p.Close())
		}
	})
	return p, name
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantError(t *testing.T, err, target error) {
	t.Helper()
	var pathErr *fs.PathError
	if !errors.Is(err, target) || !errors.As(err, &pathErr) {
		t.Fatalf("got %v, want *fs.PathError wrapping %v", err, target)
	}
}

func wantFile(t *testing.T, filesystem fs.ReadFileFS, name string, want []byte) {
	t.Helper()
	got, err := filesystem.ReadFile(name)
	must(t, err)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: content differs, got length %d, want %d", name, len(got), len(want))
	}
}

func diskSize(t *testing.T, name string) int64 {
	t.Helper()
	i, err := os.Stat(name)
	must(t, err)
	return i.Size()
}

func TestFilesAndDirectories(t *testing.T) {
	p, name := testPack(t)
	must(t, p.MkdirAll("region/deep/empty"))
	must(t, p.MkdirAll("region/deep"))
	must(t, p.MkdirAll("."))
	must(t, p.WriteFile("level.dat", []byte("level")))
	must(t, p.WriteFile("region/deep/r.0.0.mca", []byte("first")))
	must(t, p.Rename("region/deep/r.0.0.mca", "region/r.0.0.mca"))
	f, err := p.OpenFile("region/r.0.0.mca", os.O_RDWR|os.O_APPEND)
	must(t, err)
	_, err = f.Seek(0, io.SeekStart)
	must(t, err)
	_, err = f.Write([]byte(" second"))
	must(t, err)
	_, err = f.WriteAt([]byte("x"), 0)
	wantError(t, err, fs.ErrInvalid)
	must(t, f.Close())
	wantFile(t, p, "region/r.0.0.mca", []byte("first second"))
	_, err = p.OpenFile("level.dat", os.O_CREATE|os.O_EXCL|os.O_RDWR)
	wantError(t, err, fs.ErrExist)
	wantError(t, p.Mkdir("level.dat"), fs.ErrExist)
	wantError(t, p.MkdirAll("level.dat/child"), fs.ErrInvalid)
	wantError(t, p.Remove("region"), fs.ErrInvalid)
	wantError(t, p.Rename("region", "region/deep/child"), fs.ErrInvalid)
	wantError(t, p.Rename("level.dat", "region/deep"), fs.ErrExist)
	for _, invalid := range []string{"", "/level.dat", "../level.dat", "region/../level.dat", "region//bad", "region/", strings.Repeat("x", 256), "\xff"} {
		_, err := p.Open(invalid)
		wantError(t, err, fs.ErrInvalid)
	}
	must(t, p.WriteFile(strings.Repeat("x", 255), []byte("long name")))
	must(t, p.Remove(strings.Repeat("x", 255)))
	i, err := p.Stat("level.dat")
	must(t, err)
	if !i.ModTime().IsZero() || i.Mode() != 0 || i.Sys() != nil {
		t.Fatalf("unexpected attributes: %+v", i)
	}
	must(t, fstest.TestFS(p, "level.dat", "region", "region/deep", "region/deep/empty", "region/r.0.0.mca"))
	dir, err := p.Open("region")
	must(t, err)
	entries, err := dir.(fs.ReadDirFile).ReadDir(1)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "deep" {
		t.Fatalf("unexpected directory entries: %v", entries)
	}
	entries, err = dir.(fs.ReadDirFile).ReadDir(1)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "r.0.0.mca" {
		t.Fatalf("unexpected directory entries: %v", entries)
	}
	_, err = dir.(fs.ReadDirFile).ReadDir(1)
	if err != io.EOF {
		t.Fatalf("directory exhaustion: %v", err)
	}
	must(t, dir.Close())
	must(t, p.Remove("region/deep/empty"))
	must(t, p.Close())
	p, err = Open(name)
	must(t, err)
	defer p.Close()
	wantFile(t, p, "region/r.0.0.mca", []byte("first second"))
	must(t, fstest.TestFS(p, "level.dat", "region", "region/deep", "region/r.0.0.mca"))
}

func TestHandleIdentity(t *testing.T) {
	p, _ := testPack(t)
	must(t, p.Mkdir("dir"))
	data := bytes.Repeat([]byte("old"), blockSize)
	must(t, p.WriteFile("dir/source", data))
	a, err := p.OpenFile("dir/source", os.O_RDWR)
	must(t, err)
	b, err := p.OpenFile("dir/source", os.O_RDWR)
	must(t, err)
	must(t, p.WriteFile("target", []byte("target")))
	target, err := p.OpenFile("target", os.O_RDWR)
	must(t, err)
	must(t, p.Commit())
	must(t, p.Rename("dir", "moved"))
	must(t, p.Rename("moved/source", "target"))
	_, err = a.WriteAt([]byte("NEW"), 0)
	must(t, err)
	copy(data, "NEW")
	bdata, err := io.ReadAll(b)
	must(t, err)
	if !bytes.Equal(bdata, data) {
		t.Fatal("handles did not share their object")
	}
	oldTarget, err := io.ReadAll(target)
	must(t, err)
	if string(oldTarget) != "target" {
		t.Fatal("rename replacement changed the old target handle")
	}
	must(t, target.Close())
	must(t, p.Remove("target"))
	must(t, p.WriteFile("target", bytes.Repeat([]byte("replacement"), blockSize)))
	must(t, p.Commit())
	_, err = a.WriteAt([]byte("orphan"), blockSize-3)
	must(t, err)
	copy(data[blockSize-3:], "orphan")
	must(t, p.Commit())
	got := make([]byte, len(data))
	_, err = b.ReadAt(got, 0)
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("unlinked object was overwritten or reclaimed")
	}
	must(t, a.Close())
	must(t, p.Commit())
	_, err = b.ReadAt(got, 0)
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("object reclaimed before its final handle closed")
	}
	must(t, b.Close())
	must(t, p.Commit())
}

func TestContentTransitions(t *testing.T) {
	for _, size := range []int{0, 511, 512, 513, blockSize, 8 * blockSize, 8*blockSize + 1, 1024*blockSize + 37} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			p, _ := testPack(t)
			original := bytes.Repeat([]byte{0xab}, size)
			must(t, p.WriteFile("data", original))
			f, err := p.OpenFile("data", os.O_RDWR)
			must(t, err)
			defer f.Close()
			must(t, p.Commit())
			for _, shrink := range []int{size / 2, 8 * blockSize, 513, 512, 0} {
				must(t, f.Truncate(int64(shrink)))
				want := make([]byte, shrink)
				copy(want, original)
				wantFile(t, p, "data", want)
				must(t, f.Truncate(int64(size+blockSize)))
				grown := make([]byte, size+blockSize)
				copy(grown, want)
				wantFile(t, p, "data", grown)
				must(t, p.Commit())
				must(t, p.WriteFile("data", original))
				must(t, p.Commit())
			}
		})
	}
}

func TestRandomOverwriteAndSparseGaps(t *testing.T) {
	p, name := testPack(t)
	size := 1026*blockSize + 17
	want := make([]byte, size)
	rng := rand.New(rand.NewSource(23))
	_, err := rng.Read(want)
	must(t, err)
	must(t, p.WriteFile("region.mca", want))
	f, err := p.OpenFile("region.mca", os.O_RDWR)
	must(t, err)
	must(t, p.Commit())
	for i := 0; i < 120; i++ {
		count := 1 + rng.Intn(2*blockSize)
		off := rng.Intn(size - count)
		if i == 0 {
			off, count = 1024*blockSize-37, 91
		}
		data := make([]byte, count)
		_, err := rng.Read(data)
		must(t, err)
		n, err := f.WriteAt(data, int64(off))
		must(t, err)
		if n != len(data) {
			t.Fatal("short write")
		}
		copy(want[off:], data)
		if i%20 == 0 {
			must(t, f.Sync())
		}
	}
	wantFile(t, p, "region.mca", want)
	must(t, f.Close())
	must(t, p.Close())
	p, err = Open(name)
	must(t, err)
	defer p.Close()
	wantFile(t, p, "region.mca", want)
	f, err = p.OpenFile("region.mca", os.O_RDWR)
	must(t, err)
	defer f.Close()
	must(t, f.Truncate(257))
	_, err = f.WriteAt([]byte("tail"), 1024*blockSize+11)
	must(t, err)
	gap := make([]byte, 1024*blockSize+15)
	copy(gap, want[:257])
	copy(gap[1024*blockSize+11:], "tail")
	wantFile(t, p, "region.mca", gap)
}

func TestMaximumSparseLength(t *testing.T) {
	p, _ := testPack(t)
	f, err := p.Create("huge")
	must(t, err)
	defer f.Close()
	_, err = f.WriteAt([]byte{0x7f}, math.MaxUint32-1)
	must(t, err)
	must(t, f.Sync())
	buf := make([]byte, 2)
	_, err = f.ReadAt(buf, math.MaxUint32-2)
	must(t, err)
	if !bytes.Equal(buf, []byte{0, 0x7f}) {
		t.Fatalf("last mapping slot: %v", buf)
	}
	_, err = f.WriteAt([]byte{1}, math.MaxUint32)
	wantError(t, err, fs.ErrInvalid)
	wantError(t, f.Truncate(int64(math.MaxUint32)+1), fs.ErrInvalid)
	must(t, f.Truncate(1024*blockSize))
	must(t, f.Truncate(math.MaxUint32))
	_, err = f.ReadAt(buf, math.MaxUint32-2)
	must(t, err)
	if !bytes.Equal(buf, []byte{0, 0}) {
		t.Fatal("discarded final mapping reappeared")
	}
}

func TestNamespaceTreeAndRemoveAllReclaims(t *testing.T) {
	p, name := testPack(t)
	must(t, p.MkdirAll("tree/sub/deep"))
	const count = 300
	for i := 0; i < count; i++ {
		base := fmt.Sprintf("%04d%s", i, strings.Repeat("k", 230))
		must(t, p.WriteFile("tree/sub/deep/"+base, bytes.Repeat([]byte{byte(i)}, 500)))
	}
	must(t, p.WriteFile("tree/large", bytes.Repeat([]byte("x"), 300*blockSize)))
	must(t, p.WriteFile("keep", []byte("keep")))
	must(t, p.Commit())
	before := diskSize(t, name)
	entries, err := p.ReadDir("tree/sub/deep")
	must(t, err)
	if len(entries) != count {
		t.Fatalf("range across leaves: got %d entries", len(entries))
	}
	for i := 0; i < count; i += 2 {
		base := fmt.Sprintf("%04d%s", i, strings.Repeat("k", 230))
		must(t, p.Remove("tree/sub/deep/"+base))
	}
	must(t, p.Commit())
	entries, err = p.ReadDir("tree/sub/deep")
	must(t, err)
	if len(entries) != count/2 {
		t.Fatal("range after merging leaves lost entries")
	}
	must(t, p.RemoveAll("tree"))
	must(t, p.RemoveAll("tree"))
	must(t, p.Commit())
	objects, dirents := 0, 0
	must(t, p.store.treeRange(p.root, nil, func(k, _ []byte) error {
		if k[0] == 'o' {
			objects++
		} else {
			dirents++
		}
		return nil
	}))
	if objects != 2 || dirents != 1 {
		t.Fatalf("RemoveAll left records: %d objects, %d dirents", objects, dirents)
	}
	if after := diskSize(t, name); after >= before/4 {
		t.Fatalf("space not reclaimed: before=%d after=%d", before, after)
	}
	must(t, p.WriteFile("again", bytes.Repeat([]byte("y"), 200*blockSize)))
	must(t, p.Commit())
	if after := diskSize(t, name); after >= before {
		t.Fatalf("freed space was not reused: before=%d after=%d", before, after)
	}
	must(t, p.Close())
	p, err = Open(name)
	must(t, err)
	defer p.Close()
	wantFile(t, p, "keep", []byte("keep"))
	must(t, fstest.TestFS(p, "keep", "again"))
}

func TestSnapshotsRestoreAndViewLifetime(t *testing.T) {
	p, _ := testPack(t)
	old := bytes.Repeat([]byte("a"), 10*blockSize)
	must(t, p.Mkdir("dir"))
	must(t, p.WriteFile("dir/data", old))
	must(t, p.WriteFile("inline", []byte("inline")))
	must(t, p.Snapshot("base"))
	baseRoot := p.root
	if p.snapshotIndex["base"] != baseRoot {
		t.Fatal("snapshot copied the namespace instead of sharing its root")
	}
	v, err := p.SnapshotFS("base")
	must(t, err)
	must(t, fstest.TestFS(v, "dir", "dir/data", "inline"))
	f, err := p.OpenFile("dir/data", os.O_RDWR)
	must(t, err)
	_, err = f.WriteAt([]byte("new"), blockSize+10)
	must(t, err)
	wantError(t, p.Restore("base"), fs.ErrInvalid)
	wantError(t, p.Compact(), fs.ErrInvalid)
	must(t, f.Close())
	newData := bytes.Clone(old)
	copy(newData[blockSize+10:], "new")
	must(t, p.Snapshot("changed"))
	baseObj, err := p.resolve(baseRoot, "dir/data", false)
	must(t, err)
	currentObj, err := p.resolve(p.root, "dir/data", true)
	must(t, err)
	for _, index := range []uint32{0, 1, 2} {
		a, err := p.store.dataID(baseObj, index)
		must(t, err)
		b, err := p.store.dataID(currentObj, index)
		must(t, err)
		if (a == b) != (index != 1) {
			t.Fatal("CoW did not preserve unchanged data block sharing")
		}
	}
	wantFile(t, v, "dir/data", old)
	must(t, p.DeleteSnapshot("base"))
	if !reflect.DeepEqual(p.Snapshots(), []string{"changed"}) {
		t.Fatalf("snapshots: %v", p.Snapshots())
	}
	must(t, p.WriteFile("dir/data", bytes.Repeat([]byte("overwrite"), 4*blockSize)))
	must(t, p.Commit())
	wantFile(t, v, "dir/data", old)
	viewFile, err := v.Open("dir/data")
	must(t, err)
	must(t, v.Close())
	_, err = v.ReadFile("dir/data")
	wantError(t, err, fs.ErrClosed)
	must(t, p.Commit())
	viewBytes, err := io.ReadAll(viewFile)
	must(t, err)
	if !bytes.Equal(viewBytes, old) {
		t.Fatal("closing view reclaimed an open snapshot file")
	}
	must(t, viewFile.Close())
	must(t, p.RemoveAll("dir"))
	must(t, p.WriteFile("uncommitted", []byte("discard")))
	must(t, p.Restore("changed"))
	wantFile(t, p, "dir/data", newData)
	_, err = p.Stat("uncommitted")
	wantError(t, err, fs.ErrNotExist)
	must(t, p.DeleteSnapshot("changed"))
	wantFile(t, p, "dir/data", newData)
}

func TestCompactPreservesSharingAndShrinks(t *testing.T) {
	p, name := testPack(t)
	must(t, p.WriteFile("trash", bytes.Repeat([]byte("trash"), 100*blockSize)))
	must(t, p.Commit())
	data := bytes.Repeat([]byte("data"), 100*blockSize)
	must(t, p.WriteFile("keep", data))
	must(t, p.WriteFile("inline", []byte("small")))
	must(t, p.WriteFile("direct", bytes.Repeat([]byte("d"), 2*blockSize)))
	must(t, p.Remove("trash"))
	must(t, p.Snapshot("old"))
	f, err := p.OpenFile("keep", os.O_RDWR)
	must(t, err)
	_, err = f.WriteAt([]byte("new"), 17)
	must(t, err)
	must(t, f.Close())
	must(t, p.Commit())
	before := diskSize(t, name)
	must(t, p.Compact())
	if after := diskSize(t, name); after >= before {
		t.Fatalf("compact did not shrink: %d -> %d", before, after)
	}
	v, err := p.SnapshotFS("old")
	must(t, err)
	wantError(t, p.Compact(), fs.ErrInvalid)
	wantFile(t, v, "keep", data)
	copy(data[17:], "new")
	wantFile(t, p, "keep", data)
	oldObj, err := p.resolve(v.root, "keep", false)
	must(t, err)
	currentObj, err := p.resolve(p.root, "keep", true)
	must(t, err)
	a, err := p.store.dataID(oldObj, 1)
	must(t, err)
	b, err := p.store.dataID(currentObj, 1)
	must(t, err)
	if a != b {
		t.Fatal("compaction duplicated a shared data block")
	}
	must(t, v.Close())
	must(t, fstest.TestFS(p, "keep", "inline", "direct"))
	must(t, p.Close())
	must(t, os.WriteFile(name+".packfs.tmp", []byte("interrupted compact"), 0600))
	p, err = Open(name)
	must(t, err)
	defer p.Close()
	wantFile(t, p, "keep", data)
	v, err = p.SnapshotFS("old")
	must(t, err)
	defer v.Close()
	data[17], data[18], data[19] = 'a', 't', 'a'
	wantFile(t, v, "keep", data)
	wantFile(t, p, "inline", []byte("small"))
	wantFile(t, p, "direct", bytes.Repeat([]byte("d"), 2*blockSize))
	files, err := os.ReadDir(filepath.Dir(name))
	must(t, err)
	if len(files) != 1 || files[0].Name() != filepath.Base(name) {
		t.Fatalf("unexpected sidecars: %v", files)
	}
}
