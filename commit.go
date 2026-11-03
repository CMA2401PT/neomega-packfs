package packfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io/fs"
	"os"
)

const formatVersion uint32 = 1

var superMagic = [8]byte{'P', 'A', 'C', 'K', 'F', 'S', 0, 0}

// superblock 是版本发布点；CRC 覆盖校验字段置零后的完整字段区。
type superblock struct {
	generation uint64
	root       uint32
	snapshots  uint32
	nextID     uint64
}

func (s superblock) encode() []byte {
	b := make([]byte, blockSize)
	copy(b, superMagic[:])
	put32(b, 8, formatVersion)
	binary.LittleEndian.PutUint64(b[12:20], s.generation)
	put32(b, 20, s.root)
	put32(b, 24, s.snapshots)
	binary.LittleEndian.PutUint64(b[28:36], s.nextID)
	put32(b, 36, crc32.ChecksumIEEE(b[:40]))
	return b
}

func decodeSuper(b []byte) (superblock, bool) {
	if !bytes.Equal(b[:8], superMagic[:]) || get32(b, 8) != formatVersion {
		return superblock{}, false
	}
	want := get32(b, 36)
	put32(b, 36, 0)
	if crc32.ChecksumIEEE(b[:40]) != want {
		return superblock{}, false
	}
	return superblock{
		generation: binary.LittleEndian.Uint64(b[12:20]),
		root:       get32(b, 20),
		snapshots:  get32(b, 24),
		nextID:     binary.LittleEndian.Uint64(b[28:36]),
	}, true
}

// PackFS 把目录树保存在单个容器内，通过 Commit 发布工作状态。
// 实例及其文件和视图由同一个 goroutine 使用。
type PackFS struct {
	store *blockStore
	file  *os.File
	path  string
	superblock
	committed     superblock
	objects       map[uint64]*object
	files         map[*File]struct{}
	views         map[*View]struct{}
	snapshotIndex map[string]uint32
	fault         error
	closed        bool
}

// Open 创建或打开单文件容器，并清理上次压缩留下的固定临时文件。
func Open(path string) (*PackFS, error) {
	if err := os.Remove(path + ".packfs.tmp"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	p, err := openStore(f, info.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	p.file = f
	p.path = path
	return p, nil
}

func openStore(f blockIO, size int64) (*PackFS, error) {
	p := &PackFS{
		store:         newStore(f, size),
		objects:       make(map[uint64]*object),
		files:         make(map[*File]struct{}),
		views:         make(map[*View]struct{}),
		snapshotIndex: make(map[string]uint32),
	}
	p.store.mark(0)
	p.store.mark(1)
	p.store.onWriteError = p.fail
	if size == 0 {
		p.nextID = rootID + 1
		o := &object{id: rootID, parent: rootID, dir: true, linked: true}
		if err := p.saveObject(o); err != nil {
			return nil, err
		}
		if err := p.commit(true); err != nil {
			return nil, err
		}
		return p, nil
	}
	found := false
	for i := 0; i < 2; i++ {
		b := make([]byte, blockSize)
		if _, err := f.ReadAt(b, int64(i)*blockSize); err != nil {
			continue
		}
		s, ok := decodeSuper(b)
		if ok && (!found || s.generation > p.generation) {
			p.superblock = s
			found = true
		}
	}
	if !found {
		return nil, errors.New("packfs: no valid superblock")
	}
	p.committed = p.superblock
	if err := p.rebuild(); err != nil {
		return nil, err
	}
	if err := p.readSnapshots(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PackFS) mutable(op, name string) error {
	if p.fault != nil {
		return p.fault
	}
	if p.closed {
		return pathError(op, name, fs.ErrClosed)
	}
	return nil
}

func (p *PackFS) fail(err error) error {
	if err != nil && p.fault == nil {
		p.fault = err
	}
	return p.fault
}

// changeError 保留持久化故障的同一个错误值，避免外层路径包装改变它的身份。
func (p *PackFS) changeError(op, name string, err error) error {
	if p.fault != nil {
		return p.fault
	}
	return pathError(op, name, err)
}

// Commit 先同步块，再同步发布新根的超级块，最后才释放旧版本独占的空间。
func (p *PackFS) Commit() error {
	return p.commit(false)
}

// commit 把全量回收留给显式版本切换，普通提交只处理本事务撤下的块。
func (p *PackFS) commit(fullScan bool) error {
	if err := p.mutable("commit", p.path); err != nil {
		return err
	}
	s := p.store
	modified := len(s.dirty) != 0 || len(s.owned) != 0 || p.superblock != p.committed
	if !modified && !fullScan && len(s.freed) == 0 {
		return nil
	}
	if modified {
		if err := s.flush(); err != nil {
			return err
		}
		if err := s.io.Sync(); err != nil {
			return p.fail(err)
		}
		next := p.superblock
		next.generation++
		if err := writeFullAt(s.io, next.encode(), int64(next.generation%2)*blockSize); err != nil {
			return p.fail(err)
		}
		if err := s.io.Sync(); err != nil {
			return p.fail(err)
		}
		p.superblock = next
		p.committed = next
		s.owned = make(map[uint32]struct{})
	}
	if !fullScan {
		s.reclaim()
		fullScan = s.oldGarbage > s.usedCount/4
	}
	if fullScan {
		if err := p.rebuild(); err != nil {
			return p.fail(err)
		}
	}
	return p.trim()
}

// trim 只在实际存在空闲尾部时截断，避免每次小提交都调用 Truncate。
func (p *PackFS) trim() error {
	size := int64(p.store.end) * blockSize
	if size < p.store.diskSize {
		if err := p.store.io.Truncate(size); err != nil {
			return p.fail(err)
		}
		p.store.diskSize = size
	}
	return nil
}

// Close 在故障后只关闭底层文件，避免重复发布部分完成的事务。
func (p *PackFS) Close() error {
	if p.closed {
		return p.fault
	}
	err := p.fault
	if err == nil {
		err = p.Commit()
	}
	p.closed = true
	if p.file != nil {
		if closeErr := p.file.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// blockScanner 只读取树页和映射页，数据块仅标记地址；共享页只扫描一次。
type blockScanner struct {
	s      *blockStore
	seen   map[uint32]bool
	bitmap []uint64
	end    uint64
	count  uint64
}

func (v *blockScanner) mark(id uint32) {
	for int(id/64) >= len(v.bitmap) {
		v.bitmap = append(v.bitmap, 0)
	}
	mask := uint64(1) << (id % 64)
	if v.bitmap[id/64]&mask == 0 {
		v.bitmap[id/64] |= mask
		v.count++
	}
	if uint64(id) >= v.end {
		v.end = uint64(id) + 1
	}
}

func (v *blockScanner) enter(id uint32) bool {
	if id == 0 || v.seen[id] {
		return false
	}
	v.seen[id] = true
	v.mark(id)
	return true
}

func (v *blockScanner) content(o *object) error {
	if o.size <= 512 {
		return nil
	}
	if o.size <= 8*blockSize {
		for _, id := range o.direct {
			if id != 0 {
				v.mark(id)
			}
		}
		return nil
	}
	if !v.enter(o.indirect) {
		return nil
	}
	a, err := v.s.read(o.indirect)
	if err != nil {
		return err
	}
	for i := 0; i < 1024; i++ {
		id := get32(a, i*4)
		if !v.enter(id) {
			continue
		}
		b, err := v.s.read(id)
		if err != nil {
			return err
		}
		for j := 0; j < 1024; j++ {
			data := get32(b, j*4)
			if data != 0 {
				v.mark(data)
			}
		}
	}
	return nil
}

func (v *blockScanner) tree(root uint32, snapshots bool) error {
	if !v.enter(root) {
		return nil
	}
	n, err := v.s.node(root)
	if err != nil {
		return err
	}
	if !n.leaf {
		for _, id := range n.children {
			if err := v.tree(id, snapshots); err != nil {
				return err
			}
		}
		return nil
	}
	for i, key := range n.keys {
		if snapshots {
			if err := v.tree(get32(n.values[i], 0), false); err != nil {
				return err
			}
		} else if key[0] == 'o' {
			if err := v.content(decodeObject(binary.BigEndian.Uint64(key[1:]), n.values[i])); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *PackFS) rebuild() error {
	v := &blockScanner{s: p.store, seen: make(map[uint32]bool)}
	v.mark(0)
	v.mark(1)
	if err := v.tree(p.root, false); err != nil {
		return err
	}
	if err := v.tree(p.snapshots, true); err != nil {
		return err
	}
	for view := range p.views {
		if err := v.tree(view.root, false); err != nil {
			return err
		}
	}
	for f := range p.files {
		if f.readOnly {
			if err := v.tree(f.root, false); err != nil {
				return err
			}
		} else if err := v.content(f.obj); err != nil {
			return err
		}
	}
	p.store.used = v.bitmap
	p.store.hint = 2
	p.store.end = v.end
	p.store.usedCount = v.count
	p.store.young = make(map[uint32]struct{})
	p.store.freed = make(map[uint32]struct{})
	p.store.oldGarbage = 0
	return nil
}
