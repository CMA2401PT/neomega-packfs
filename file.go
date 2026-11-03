package packfs

import (
	"io"
	"io/fs"
	"math"
	"os"
)

// File 绑定对象而非路径；快照句柄另外保留自己的命名空间根。
type File struct {
	p          *PackFS
	obj        *object
	name       string
	offset     int64
	flag       int
	readOnly   bool
	root       uint32
	closed     bool
	dirEntries []fs.DirEntry
	dirOffset  int
}

var (
	_ fs.FS          = (*PackFS)(nil)
	_ fs.ReadDirFS   = (*PackFS)(nil)
	_ fs.StatFS      = (*PackFS)(nil)
	_ fs.ReadFileFS  = (*PackFS)(nil)
	_ fs.ReadDirFile = (*File)(nil)
	_ io.ReaderAt    = (*File)(nil)
	_ io.WriterAt    = (*File)(nil)
	_ io.Seeker      = (*File)(nil)
)

func (p *PackFS) Open(name string) (fs.File, error) {
	f, err := p.OpenFile(name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return f, nil
}
func (p *PackFS) Create(name string) (*File, error) {
	return p.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
}

func (p *PackFS) OpenFile(name string, flag int) (*File, error) {
	writes := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if writes {
		if err := p.mutable("open", name); err != nil {
			return nil, err
		}
	} else if p.closed {
		return nil, pathError("open", name, fs.ErrClosed)
	}
	o, err := p.resolve(p.root, name, true)
	if err == fs.ErrNotExist && flag&os.O_CREATE != 0 {
		parent, base, parentErr := p.parent(name)
		if parentErr != nil {
			return nil, pathError("open", name, parentErr)
		}
		o, err = p.makeObject(parent, base, false)
	} else if err == nil && flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
		err = fs.ErrExist
	}
	if err != nil {
		if writes {
			return nil, p.changeError("open", name, err)
		}
		return nil, pathError("open", name, err)
	}
	if o.dir && writes {
		return nil, pathError("open", name, fs.ErrInvalid)
	}
	if flag&os.O_TRUNC != 0 {
		if flag&(os.O_RDWR|os.O_WRONLY) == 0 {
			return nil, pathError("open", name, fs.ErrPermission)
		}
		if err := p.store.resizeContent(o, 0); err != nil {
			return nil, p.fail(err)
		}
		if err := p.saveObject(o); err != nil {
			return nil, p.changeError("open", name, err)
		}
	}
	p.objects[o.id] = o
	f := &File{p: p, obj: o, name: name, flag: flag}
	p.files[f] = struct{}{}
	return f, nil
}

func (p *PackFS) WriteFile(name string, data []byte) error {
	f, err := p.Create(name)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (f *File) readable(op string) error {
	if f.closed || f.p.closed {
		return pathError(op, f.name, fs.ErrClosed)
	}
	if f.obj.dir {
		return pathError(op, f.name, fs.ErrInvalid)
	}
	if f.flag&os.O_WRONLY != 0 {
		return pathError(op, f.name, fs.ErrPermission)
	}
	return nil
}

func (f *File) writable(op string) error {
	if err := f.p.mutable(op, f.name); err != nil {
		return err
	}
	if f.closed {
		return pathError(op, f.name, fs.ErrClosed)
	}
	if f.readOnly || f.flag&(os.O_RDWR|os.O_WRONLY) == 0 {
		return pathError(op, f.name, fs.ErrPermission)
	}
	if f.obj.dir {
		return pathError(op, f.name, fs.ErrInvalid)
	}
	return nil
}

func (f *File) ReadAt(b []byte, off int64) (int, error) {
	if err := f.readable("read"); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, pathError("read", f.name, fs.ErrInvalid)
	}
	n, err := f.p.store.readContent(f.obj, b, off)
	if err != nil && err != io.EOF {
		err = pathError("read", f.name, err)
	}
	return n, err
}

func (f *File) Read(b []byte) (int, error) {
	n, err := f.ReadAt(b, f.offset)
	f.offset += int64(n)
	return n, err
}

func (f *File) writeAt(b []byte, off int64) (int, error) {
	if off < 0 || off > math.MaxUint32 || int64(len(b)) > math.MaxUint32-off {
		return 0, pathError("write", f.name, fs.ErrInvalid)
	}
	n, err := f.p.store.writeContent(f.obj, b, off)
	if err != nil {
		return n, f.p.fail(err)
	}
	err = f.p.saveObject(f.obj)
	return n, f.p.changeError("write", f.name, err)
}

func (f *File) WriteAt(b []byte, off int64) (int, error) {
	if err := f.writable("write"); err != nil {
		return 0, err
	}
	if f.flag&os.O_APPEND != 0 {
		return 0, pathError("write", f.name, fs.ErrInvalid)
	}
	return f.writeAt(b, off)
}

func (f *File) Write(b []byte) (int, error) {
	if err := f.writable("write"); err != nil {
		return 0, err
	}
	off := f.offset
	if f.flag&os.O_APPEND != 0 {
		off = int64(f.obj.size)
	}
	n, err := f.writeAt(b, off)
	f.offset = off + int64(n)
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	if f.closed || f.p.closed {
		return 0, pathError("seek", f.name, fs.ErrClosed)
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.offset
	case io.SeekEnd:
		base = int64(f.obj.size)
	default:
		return 0, pathError("seek", f.name, fs.ErrInvalid)
	}
	if offset < -base || offset > math.MaxInt64-base {
		return 0, pathError("seek", f.name, fs.ErrInvalid)
	}
	f.offset = base + offset
	return f.offset, nil
}

func (f *File) Truncate(size int64) error {
	if err := f.writable("truncate"); err != nil {
		return err
	}
	if size < 0 || size > math.MaxUint32 {
		return pathError("truncate", f.name, fs.ErrInvalid)
	}
	if err := f.p.store.resizeContent(f.obj, uint32(size)); err != nil {
		return f.p.fail(err)
	}
	return f.p.changeError("truncate", f.name, f.p.saveObject(f.obj))
}

// Sync 提交整个容器，因此同一 PackFS 中其他文件的工作状态也会一起持久化。
func (f *File) Sync() error {
	if f.p.fault != nil {
		return f.p.fault
	}
	if f.closed || f.p.closed {
		return pathError("sync", f.name, fs.ErrClosed)
	}
	if f.readOnly {
		return pathError("sync", f.name, fs.ErrPermission)
	}
	return f.p.Commit()
}

func (f *File) Stat() (fs.FileInfo, error) {
	if f.closed || f.p.closed {
		return nil, pathError("stat", f.name, fs.ErrClosed)
	}
	return infoFor(f.name, f.obj), nil
}

func (f *File) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed || f.p.closed {
		return nil, pathError("readdir", f.name, fs.ErrClosed)
	}
	if !f.obj.dir {
		return nil, pathError("readdir", f.name, fs.ErrInvalid)
	}
	if f.dirEntries == nil {
		root := f.p.root
		if f.readOnly {
			root = f.root
		}
		var err error
		f.dirEntries, err = f.p.entries(root, f.obj, !f.readOnly)
		if err != nil {
			return nil, pathError("readdir", f.name, err)
		}
	}
	end := len(f.dirEntries)
	if n > 0 {
		if f.dirOffset == end {
			return nil, io.EOF
		}
		end = min(end, f.dirOffset+n)
	}
	entries := f.dirEntries[f.dirOffset:end]
	f.dirOffset = end
	return entries, nil
}

func (f *File) Close() error {
	if f.closed {
		return pathError("close", f.name, fs.ErrClosed)
	}
	f.closed = true
	delete(f.p.files, f)
	if !f.readOnly {
		keep := false
		for other := range f.p.files {
			if !other.readOnly && other.obj == f.obj {
				keep = true
				break
			}
		}
		if !keep {
			delete(f.p.objects, f.obj.id)
			if !f.obj.linked && !f.p.closed && f.p.fault == nil {
				return pathError("close", f.name, f.p.store.freeContent(f.obj))
			}
		}
	}
	return nil
}
