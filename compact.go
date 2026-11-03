package packfs

import (
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
)

// pageKind 区分复制时需要重写的引用，数据块的字节可以直接搬运。
type pageKind uint8

const (
	dataPage pageKind = iota
	namespacePage
	snapshotPage
	firstMapPage
	secondMapPage
)

// blockCopier 用物理地址去重，重写引用时保留当前树与快照之间的共享。
type blockCopier struct {
	source *blockStore
	target blockIO
	moved  map[uint32]uint32
	next   uint32
}

func (c *blockCopier) copyBlock(id uint32, kind pageKind) (uint32, error) {
	if id == 0 {
		return 0, nil
	}
	if next, ok := c.moved[id]; ok {
		return next, nil
	}
	next := c.next
	c.next++
	c.moved[id] = next
	old, err := c.source.read(id)
	if err != nil {
		return 0, err
	}
	b := append([]byte(nil), old...)
	switch kind {
	case namespacePage, snapshotPage:
		n := decodeNode(b)
		if !n.leaf {
			for i, child := range n.children {
				n.children[i], err = c.copyBlock(child, kind)
				if err != nil {
					return 0, err
				}
			}
		} else {
			for i, key := range n.keys {
				if kind == snapshotPage {
					root, err := c.copyBlock(get32(n.values[i], 0), namespacePage)
					if err != nil {
						return 0, err
					}
					put32(n.values[i], 0, root)
				} else if key[0] == 'o' {
					o := decodeObject(binary.BigEndian.Uint64(key[1:]), n.values[i])
					if o.size > 8*blockSize {
						o.indirect, err = c.copyBlock(o.indirect, firstMapPage)
					} else if o.size > 512 {
						for j, data := range o.direct {
							o.direct[j], err = c.copyBlock(data, dataPage)
							if err != nil {
								return 0, err
							}
						}
					}
					if err != nil {
						return 0, err
					}
					n.values[i] = o.encode()
				}
			}
		}
		b = n.encode()
	case firstMapPage, secondMapPage:
		childKind := dataPage
		if kind == firstMapPage {
			childKind = secondMapPage
		}
		for i := 0; i < 1024; i++ {
			child, err := c.copyBlock(get32(b, i*4), childKind)
			if err != nil {
				return 0, err
			}
			put32(b, i*4, child)
		}
	}
	if err := writeFullAt(c.target, b, int64(next)*blockSize); err != nil {
		return 0, err
	}
	return next, nil
}

// Compact 先构造并同步完整替代文件，替换并同步目录后才切换实例的底层文件。
func (p *PackFS) Compact() error {
	if err := p.mutable("compact", p.path); err != nil {
		return err
	}
	if len(p.files) != 0 || len(p.views) != 0 {
		return pathError("compact", p.path, fs.ErrInvalid)
	}
	if err := p.Commit(); err != nil {
		return err
	}
	tempPath := p.path + ".packfs.tmp"
	temp, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			temp.Close()
		}
		os.Remove(tempPath)
	}()
	c := &blockCopier{source: p.store, target: temp, moved: make(map[uint32]uint32), next: 2}
	next := p.superblock
	next.generation++
	next.root, err = c.copyBlock(p.root, namespacePage)
	if err != nil {
		return p.fail(err)
	}
	next.snapshots, err = c.copyBlock(p.snapshots, snapshotPage)
	if err != nil {
		return p.fail(err)
	}
	if err := temp.Sync(); err != nil {
		return p.fail(err)
	}
	previous := next
	previous.generation--
	if err := writeFullAt(temp, previous.encode(), int64(previous.generation%2)*blockSize); err != nil {
		return p.fail(err)
	}
	if err := writeFullAt(temp, next.encode(), int64(next.generation%2)*blockSize); err != nil {
		return p.fail(err)
	}
	if err := temp.Sync(); err != nil {
		return p.fail(err)
	}
	if err := os.Rename(tempPath, p.path); err != nil {
		return p.fail(err)
	}
	if err := syncDirectory(filepath.Dir(p.path)); err != nil {
		return p.fail(err)
	}
	old := p.file
	p.file = temp
	p.superblock = next
	p.committed = next
	p.store = newStore(temp, int64(c.next)*blockSize)
	p.store.onWriteError = p.fail
	keep = true
	for name, root := range p.snapshotIndex {
		p.snapshotIndex[name] = c.moved[root]
	}
	if err := p.rebuild(); err != nil {
		return p.fail(err)
	}
	if old != nil {
		return old.Close()
	}
	return nil
}
