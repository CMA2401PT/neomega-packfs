package packfs

import (
	"encoding/binary"
	"io/fs"
	"path"
	"strings"
	"time"
)

func objectKey(id uint64) []byte {
	b := make([]byte, 9)
	b[0] = 'o'
	binary.BigEndian.PutUint64(b[1:], id)
	return b
}
func dirKey(parent uint64, name string) []byte {
	b := objectKey(parent)
	b[0] = 'd'
	return append(b, name...)
}
func idValue(id uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, id); return b }

func pathError(op, name string, err error) error {
	if err == nil {
		return nil
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

func validPath(name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) > 255 {
			return false
		}
	}
	return true
}

func (p *PackFS) loadObject(root uint32, id uint64, working bool) (*object, error) {
	if working {
		if o := p.objects[id]; o != nil {
			return o, nil
		}
	}
	b, err := p.store.treeGet(root, objectKey(id))
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fs.ErrNotExist
	}
	return decodeObject(id, b), nil
}

func (p *PackFS) saveObject(o *object) error {
	if !o.linked {
		return p.store.spill()
	}
	r, err := p.store.treeSet(p.root, objectKey(o.id), o.encode())
	if err != nil {
		return err
	}
	p.root = r
	return nil
}

func (p *PackFS) setEntry(parent uint64, name string, id uint64) error {
	var value []byte
	if id != 0 {
		value = idValue(id)
	}
	r, err := p.store.treeSet(p.root, dirKey(parent, name), value)
	if err != nil {
		return err
	}
	p.root = r
	return nil
}

func (p *PackFS) resolve(root uint32, name string, working bool) (*object, error) {
	if !validPath(name) {
		return nil, fs.ErrInvalid
	}
	o, err := p.loadObject(root, rootID, working)
	if err != nil {
		return nil, err
	}
	if name == "." {
		return o, nil
	}
	for _, part := range strings.Split(name, "/") {
		if !o.dir {
			return nil, fs.ErrInvalid
		}
		b, err := p.store.treeGet(root, dirKey(o.id, part))
		if err != nil {
			return nil, err
		}
		if b == nil {
			return nil, fs.ErrNotExist
		}
		o, err = p.loadObject(root, binary.LittleEndian.Uint64(b), working)
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (p *PackFS) parent(name string) (*object, string, error) {
	if !validPath(name) || name == "." {
		return nil, "", fs.ErrInvalid
	}
	parent, err := p.resolve(p.root, path.Dir(name), true)
	if err == nil && !parent.dir {
		err = fs.ErrInvalid
	}
	return parent, path.Base(name), err
}

func (p *PackFS) makeObject(parent *object, name string, dir bool) (*object, error) {
	o := &object{id: p.nextID, parent: parent.id, dir: dir, linked: true}
	p.nextID++
	if err := p.saveObject(o); err != nil {
		return nil, err
	}
	if err := p.setEntry(parent.id, name, o.id); err != nil {
		return nil, err
	}
	return o, nil
}

func (p *PackFS) Mkdir(name string) error {
	if err := p.mutable("mkdir", name); err != nil {
		return err
	}
	if _, err := p.resolve(p.root, name, true); err == nil {
		return p.changeError("mkdir", name, fs.ErrExist)
	} else if err != fs.ErrNotExist {
		return p.changeError("mkdir", name, err)
	}
	parent, base, err := p.parent(name)
	if err == nil {
		_, err = p.makeObject(parent, base, true)
	}
	return p.changeError("mkdir", name, err)
}

func (p *PackFS) MkdirAll(name string) error {
	if err := p.mutable("mkdirall", name); err != nil {
		return err
	}
	if !validPath(name) {
		return p.changeError("mkdirall", name, fs.ErrInvalid)
	}
	if name == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(name, "/") {
		current = path.Join(current, part)
		o, err := p.resolve(p.root, current, true)
		if err == fs.ErrNotExist {
			if err := p.Mkdir(current); err != nil {
				return err
			}
		} else if err != nil {
			return p.changeError("mkdirall", name, err)
		} else if !o.dir {
			return p.changeError("mkdirall", name, fs.ErrInvalid)
		}
	}
	return nil
}

// entryInfo 只暴露容器保留的信息，修改时间始终为零值。
type entryInfo struct {
	name string
	size int64
	dir  bool
}

func (i entryInfo) Name() string { return i.name }
func (i entryInfo) Size() int64  { return i.size }
func (i entryInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i entryInfo) ModTime() time.Time         { return time.Time{} }
func (i entryInfo) IsDir() bool                { return i.dir }
func (i entryInfo) Sys() any                   { return nil }
func (i entryInfo) Type() fs.FileMode          { return i.Mode().Type() }
func (i entryInfo) Info() (fs.FileInfo, error) { return i, nil }
func infoFor(name string, o *object) entryInfo {
	return entryInfo{name: path.Base(name), size: int64(o.size), dir: o.dir}
}

func (p *PackFS) stat(root uint32, name string, working bool) (fs.FileInfo, error) {
	o, err := p.resolve(root, name, working)
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	return infoFor(name, o), nil
}

func (p *PackFS) Stat(name string) (fs.FileInfo, error) {
	if p.closed {
		return nil, pathError("stat", name, fs.ErrClosed)
	}
	return p.stat(p.root, name, true)
}

func (p *PackFS) entries(root uint32, o *object, working bool) ([]fs.DirEntry, error) {
	if !o.dir {
		return nil, fs.ErrInvalid
	}
	entries := make([]fs.DirEntry, 0)
	err := p.store.treeRange(root, dirKey(o.id, ""), func(k, v []byte) error {
		child, err := p.loadObject(root, binary.LittleEndian.Uint64(v), working)
		if err != nil {
			return err
		}
		entries = append(entries, infoFor(string(k[9:]), child))
		return nil
	})
	return entries, err
}

func (p *PackFS) readDir(root uint32, name string, working bool) ([]fs.DirEntry, error) {
	o, err := p.resolve(root, name, working)
	if err != nil {
		return nil, pathError("readdir", name, err)
	}
	entries, err := p.entries(root, o, working)
	return entries, pathError("readdir", name, err)
}

func (p *PackFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if p.closed {
		return nil, pathError("readdir", name, fs.ErrClosed)
	}
	return p.readDir(p.root, name, true)
}

func (p *PackFS) readFile(root uint32, name string, working bool) ([]byte, error) {
	o, err := p.resolve(root, name, working)
	if err == nil && o.dir {
		err = fs.ErrInvalid
	}
	if err != nil {
		return nil, pathError("readfile", name, err)
	}
	b := make([]byte, int(o.size))
	_, err = p.store.readContent(o, b, 0)
	return b, pathError("readfile", name, err)
}

func (p *PackFS) ReadFile(name string) ([]byte, error) {
	if p.closed {
		return nil, pathError("readfile", name, fs.ErrClosed)
	}
	return p.readFile(p.root, name, true)
}

func (p *PackFS) unlinkObject(o *object) error {
	r, err := p.store.treeSet(p.root, objectKey(o.id), nil)
	if err != nil {
		return err
	}
	p.root = r
	o.linked = false
	if p.objects[o.id] == nil {
		return p.store.freeContent(o)
	}
	return nil
}

func (p *PackFS) Remove(name string) error {
	if err := p.mutable("remove", name); err != nil {
		return err
	}
	parent, base, err := p.parent(name)
	if err != nil {
		return p.changeError("remove", name, err)
	}
	o, err := p.resolve(p.root, name, true)
	if err != nil {
		return p.changeError("remove", name, err)
	}
	if o.dir {
		entries, err := p.entries(p.root, o, true)
		if err != nil {
			return p.changeError("remove", name, err)
		}
		if len(entries) != 0 {
			return p.changeError("remove", name, fs.ErrInvalid)
		}
	}
	if err := p.setEntry(parent.id, base, 0); err != nil {
		return p.changeError("remove", name, err)
	}
	return p.changeError("remove", name, p.unlinkObject(o))
}

// removeTree 收集当前层的目录项再递归删除，避免遍历过程中改变树的搜索路径。
func (p *PackFS) removeTree(o *object) error {
	if o.dir {
		var children []struct {
			name string
			id   uint64
		}
		if err := p.store.treeRange(p.root, dirKey(o.id, ""), func(k, v []byte) error {
			children = append(children, struct {
				name string
				id   uint64
			}{string(k[9:]), binary.LittleEndian.Uint64(v)})
			return nil
		}); err != nil {
			return err
		}
		for _, child := range children {
			c, err := p.loadObject(p.root, child.id, true)
			if err != nil {
				return err
			}
			if err := p.removeTree(c); err != nil {
				return err
			}
			if err := p.setEntry(o.id, child.name, 0); err != nil {
				return err
			}
		}
	}
	return p.unlinkObject(o)
}

func (p *PackFS) RemoveAll(name string) error {
	if err := p.mutable("removeall", name); err != nil {
		return err
	}
	parent, base, err := p.parent(name)
	if err != nil {
		return p.changeError("removeall", name, err)
	}
	o, err := p.resolve(p.root, name, true)
	if err == fs.ErrNotExist {
		return nil
	}
	if err != nil {
		return p.changeError("removeall", name, err)
	}
	if err := p.removeTree(o); err != nil {
		return p.changeError("removeall", name, err)
	}
	return p.changeError("removeall", name, p.setEntry(parent.id, base, 0))
}

func (p *PackFS) Rename(oldName, newName string) error {
	wrap := func(err error) error { return p.changeError("rename", oldName+" -> "+newName, err) }
	if err := p.mutable("rename", oldName+" -> "+newName); err != nil {
		return err
	}
	oldParent, oldBase, err := p.parent(oldName)
	if err != nil {
		return wrap(err)
	}
	newParent, newBase, err := p.parent(newName)
	if err != nil {
		return wrap(err)
	}
	o, err := p.resolve(p.root, oldName, true)
	if err != nil {
		return wrap(err)
	}
	if oldName == newName {
		return nil
	}
	if o.dir {
		for ancestor := newParent; ; {
			if ancestor.id == o.id {
				return wrap(fs.ErrInvalid)
			}
			if ancestor.id == rootID {
				break
			}
			ancestor, err = p.loadObject(p.root, ancestor.parent, true)
			if err != nil {
				return wrap(err)
			}
		}
	}
	target, err := p.resolve(p.root, newName, true)
	if err == nil {
		if target.dir {
			return wrap(fs.ErrExist)
		}
		if o.dir {
			return wrap(fs.ErrInvalid)
		}
		if err := p.unlinkObject(target); err != nil {
			return wrap(err)
		}
	} else if err != fs.ErrNotExist {
		return wrap(err)
	}
	if err := p.setEntry(oldParent.id, oldBase, 0); err != nil {
		return wrap(err)
	}
	if err := p.setEntry(newParent.id, newBase, o.id); err != nil {
		return wrap(err)
	}
	o.parent = newParent.id
	return wrap(p.saveObject(o))
}
