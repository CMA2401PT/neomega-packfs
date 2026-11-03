// Package transfer 通过同一份目录遍历复制本机文件、容器内容和快照内容。
package transfer

import (
	"archive/zip"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	packfs "neomega-packfs"
)

// Destination 只负责创建目录和文件，复制过程无需知道写入的是容器、本机还是 ZIP。
type Destination interface {
	MkdirAll(string) error
	Create(string) (io.WriteCloser, error)
}

// Container 保留未经清理的路径，让库判断容器路径是否合法。
type Container struct{ FS *packfs.PackFS }

func (d Container) MkdirAll(name string) error { return d.FS.MkdirAll(name) }

func (d Container) Create(name string) (io.WriteCloser, error) {
	parent := "."
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		parent = name[:i]
	}
	if err := d.FS.MkdirAll(parent); err != nil {
		return nil, err
	}
	f, err := d.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Local 按本机路径创建输出，目标目录对应源目录本身，文件对应完整目标文件名。
type Local struct{}

func (Local) MkdirAll(name string) error { return os.MkdirAll(filepath.FromSlash(name), 0755) }

func (Local) Create(name string) (io.WriteCloser, error) {
	name = filepath.FromSlash(name)
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return nil, err
	}
	return os.Create(name)
}

// Join 不清理路径分量，避免把原本应该报错的容器路径变成另一个有效路径。
func Join(parent, child string) string {
	if parent == "." {
		return child
	}
	return parent + "/" + child
}

// WriteFile 在返回前关闭输出，使恢复和压缩不会被复制过程遗留的句柄阻挡。
func WriteFile(dst Destination, name string, src io.Reader) (err error) {
	f, err := dst.Create(name)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	_, err = io.Copy(f, src)
	return err
}

// Copy 把源节点对应到完整目标路径，目录的子项保持相对层次和空目录。
func Copy(src fs.FS, source string, dst Destination, target string) error {
	return fs.WalkDir(src, source, func(name string, entry fs.DirEntry, err error) (result error) {
		if err != nil {
			return err
		}
		dest := target
		if name != source {
			relative := name
			if source != "." {
				relative = strings.TrimPrefix(name, source+"/")
			}
			dest = Join(target, relative)
		}
		if entry.IsDir() {
			return dst.MkdirAll(dest)
		}
		f, err := src.Open(name)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := f.Close(); result == nil {
				result = closeErr
			}
		}()
		return WriteFile(dst, dest, f)
	})
}

// Import 用本机目录作为 fs.FS 的根，绝对路径和当前目录都可直接作为源。
func Import(local string, dst *packfs.PackFS, target string) error {
	abs, err := filepath.Abs(local)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return Copy(
			os.DirFS(abs),
			".",
			Container{FS: dst},
			target,
		)
	}
	return Copy(
		os.DirFS(filepath.Dir(abs)),
		filepath.Base(abs),
		Container{FS: dst},
		target,
	)
}

// Zip 用相同的递归复制过程生成目录下载，压缩包保留最外层目录名。
func Zip(w io.Writer, src fs.FS, name string) (err error) {
	z := zip.NewWriter(w)
	defer func() {
		if closeErr := z.Close(); err == nil {
			err = closeErr
		}
	}()
	base := path.Base(name)
	if name == "." {
		base = "root"
	}
	return Copy(
		src,
		name,
		archiveDestination{writer: z},
		base,
	)
}

type archiveDestination struct{ writer *zip.Writer }

func (d archiveDestination) MkdirAll(name string) error {
	header := &zip.FileHeader{Name: name + "/", Method: zip.Store}
	header.SetMode(fs.ModeDir | 0755)
	_, err := d.writer.CreateHeader(header)
	return err
}

func (d archiveDestination) Create(name string) (io.WriteCloser, error) {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0644)
	w, err := d.writer.CreateHeader(header)
	if err != nil {
		return nil, err
	}
	return archiveEntry{Writer: w}, nil
}

// archiveEntry 的结束由下一条 ZIP 记录或整个压缩包的 Close 负责。
type archiveEntry struct{ io.Writer }

func (archiveEntry) Close() error { return nil }
