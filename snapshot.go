package packfs

import (
	"io/fs"
	"sort"
	"strings"
)

func validSnapshot(name string) bool {
	return name != "." && validPath(name) && !strings.Contains(name, "/")
}

func (p *PackFS) snapshotRoot(name string) (uint32, error) {
	if !validSnapshot(name) {
		return 0, fs.ErrInvalid
	}
	root, ok := p.snapshotIndex[name]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return root, nil
}

// Snapshot 先持久化工作根，再把该根登记到快照树并发布第二次提交。
func (p *PackFS) Snapshot(name string) error {
	if err := p.mutable("snapshot", name); err != nil {
		return err
	}
	if !validSnapshot(name) {
		return p.changeError("snapshot", name, fs.ErrInvalid)
	}
	if _, err := p.snapshotRoot(name); err == nil {
		return p.changeError("snapshot", name, fs.ErrExist)
	} else if err != fs.ErrNotExist {
		return p.changeError("snapshot", name, err)
	}
	if err := p.Commit(); err != nil {
		return err
	}
	b := make([]byte, 4)
	put32(b, 0, p.root)
	r, err := p.store.treeSet(p.snapshots, []byte(name), b)
	if err != nil {
		return p.changeError("snapshot", name, err)
	}
	p.snapshots = r
	p.snapshotIndex[name] = p.root
	if err := p.Commit(); err != nil {
		return err
	}
	p.store.young = make(map[uint32]struct{})
	return nil
}

// Snapshots 从打开时载入的名字索引返回列表，枚举不需要再次读取磁盘。
func (p *PackFS) Snapshots() []string {
	names := make([]string, 0, len(p.snapshotIndex))
	for name := range p.snapshotIndex {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p *PackFS) readSnapshots() error {
	return p.store.treeRange(p.snapshots, nil, func(k, value []byte) error {
		p.snapshotIndex[string(k)] = get32(value, 0)
		return nil
	})
}

func (p *PackFS) DeleteSnapshot(name string) error {
	if err := p.mutable("deletesnapshot", name); err != nil {
		return err
	}
	if _, err := p.snapshotRoot(name); err != nil {
		return p.changeError("deletesnapshot", name, err)
	}
	r, err := p.store.treeSet(p.snapshots, []byte(name), nil)
	if err != nil {
		return p.changeError("deletesnapshot", name, err)
	}
	p.snapshots = r
	delete(p.snapshotIndex, name)
	return p.commit(true)
}

// View 保留一个只读根，删除快照名字不会影响已打开的视图。
type View struct {
	p      *PackFS
	root   uint32
	closed bool
}

var (
	_ fs.FS         = (*View)(nil)
	_ fs.ReadDirFS  = (*View)(nil)
	_ fs.StatFS     = (*View)(nil)
	_ fs.ReadFileFS = (*View)(nil)
)

func (p *PackFS) SnapshotFS(name string) (*View, error) {
	if p.closed {
		return nil, pathError("snapshotfs", name, fs.ErrClosed)
	}
	root, err := p.snapshotRoot(name)
	if err != nil {
		return nil, pathError("snapshotfs", name, err)
	}
	v := &View{p: p, root: root}
	p.views[v] = struct{}{}
	return v, nil
}

func (v *View) Open(name string) (fs.File, error) {
	if v.closed || v.p.closed {
		return nil, pathError("open", name, fs.ErrClosed)
	}
	o, err := v.p.resolve(v.root, name, false)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	f := &File{p: v.p, obj: o, name: name, root: v.root, readOnly: true}
	v.p.files[f] = struct{}{}
	return f, nil
}

func (v *View) Stat(name string) (fs.FileInfo, error) {
	if v.closed || v.p.closed {
		return nil, pathError("stat", name, fs.ErrClosed)
	}
	return v.p.stat(v.root, name, false)
}

func (v *View) ReadDir(name string) ([]fs.DirEntry, error) {
	if v.closed || v.p.closed {
		return nil, pathError("readdir", name, fs.ErrClosed)
	}
	return v.p.readDir(v.root, name, false)
}

func (v *View) ReadFile(name string) ([]byte, error) {
	if v.closed || v.p.closed {
		return nil, pathError("readfile", name, fs.ErrClosed)
	}
	return v.p.readFile(v.root, name, false)
}

func (v *View) Close() error {
	if v.closed {
		return fs.ErrClosed
	}
	v.closed = true
	delete(v.p.views, v)
	return nil
}

// Restore 丢弃工作树而保留快照树；打开的视图仍各自引用原来的根。
func (p *PackFS) Restore(name string) error {
	if err := p.mutable("restore", name); err != nil {
		return err
	}
	if len(p.files) != 0 {
		return pathError("restore", name, fs.ErrInvalid)
	}
	root, err := p.snapshotRoot(name)
	if err != nil {
		return pathError("restore", name, err)
	}
	p.root = root
	p.objects = make(map[uint64]*object)
	p.store.dirty = make(map[uint32][]byte)
	p.store.owned = make(map[uint32]struct{})
	p.store.freed = make(map[uint32]struct{})
	return p.commit(true)
}
