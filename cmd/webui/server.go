package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"

	packfs "neomega-packfs"
	"neomega-packfs/cmd/internal/transfer"
)

//go:embed static/*
var assets embed.FS

// server 用同一把锁覆盖请求中的全部容器操作，包括复制、下载和句柄关闭。
type server struct {
	mu       sync.Mutex
	pack     *packfs.PackFS
	filename string
	hosts    map[string]bool
}

// openServer 固定监听地址和本机别名，避免恶意域名借 DNS 重绑定访问容器。
func openServer(filename, addr string) (*server, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	p, err := packfs.Open(filename)
	if err != nil {
		return nil, err
	}
	return &server{
		pack:     p,
		filename: filename,
		hosts: map[string]bool{
			addr:                                true,
			net.JoinHostPort("localhost", port): true,
			net.JoinHostPort("127.0.0.1", port): true,
			net.JoinHostPort("::1", port):       true,
		},
	}, nil
}

func (s *server) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pack.Close()
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/list", s.locked(s.list, false))
	mux.HandleFunc("GET /api/download", s.locked(s.download, false))
	mux.HandleFunc("POST /api/upload", s.locked(s.upload, true))
	mux.HandleFunc("POST /api/mkdir", s.locked(s.mkdir, true))
	mux.HandleFunc("POST /api/remove", s.locked(s.remove, true))
	mux.HandleFunc("POST /api/rename", s.locked(s.rename, true))
	mux.HandleFunc("POST /api/snapshot/create", s.locked(s.snapshotCreate, true))
	mux.HandleFunc("POST /api/snapshot/delete", s.locked(s.snapshotDelete, true))
	mux.HandleFunc("POST /api/snapshot/restore", s.locked(s.snapshotRestore, true))
	mux.HandleFunc("POST /api/compact", s.locked(s.compact, true))
	return mux
}

type action func(http.ResponseWriter, *http.Request) error

// locked 在操作返回、句柄关闭后提交，保证每个修改请求都有明确的持久化边界。
// Host 限制访问入口，写请求的自定义头使浏览器跨站写入必须先通过预检。
func (s *server) locked(fn action, write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[r.Host] || (write && r.Header.Get("X-PackFS") != "1") {
			respondError(w, fs.ErrPermission)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if write && r.URL.Query().Get("snapshot") != "" {
			respondError(w, fs.ErrPermission)
			return
		}
		err := fn(w, r)
		if write {
			if commitErr := s.pack.Commit(); err == nil {
				err = commitErr
			}
		}
		if err != nil {
			respondError(w, err)
			return
		}
		if write {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func respondError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, fs.ErrExist):
		status = http.StatusConflict
	case errors.Is(err, fs.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, fs.ErrPermission):
		status = http.StatusForbidden
	}
	http.Error(w, err.Error(), status)
}

func readJSON(r *http.Request, value any) error {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		return fmt.Errorf("%w: %v", fs.ErrInvalid, err)
	}
	return nil
}

// source 只在单个请求内保留快照根，页面切换不会长期占用视图或文件句柄。
func (s *server) source(snapshot string) (fs.FS, func() error, error) {
	if snapshot == "" {
		return s.pack, func() error { return nil }, nil
	}
	v, err := s.pack.SnapshotFS(snapshot)
	if err != nil {
		return nil, nil, err
	}
	return v, v.Close, nil
}

type entry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
}

type listing struct {
	Path      string   `json:"path"`
	Entries   []entry  `json:"entries"`
	Snapshots []string `json:"snapshots"`
	Container string   `json:"container"`
	Size      int64    `json:"size"`
}

func (s *server) list(w http.ResponseWriter, r *http.Request) (err error) {
	name := r.URL.Query().Get("path")
	if name == "" {
		name = "."
	}
	source, closeSource, err := s.source(r.URL.Query().Get("snapshot"))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeSource(); err == nil {
			err = closeErr
		}
	}()
	children, err := fs.ReadDir(source, name)
	if err != nil {
		return err
	}
	info, err := os.Stat(s.filename)
	if err != nil {
		return err
	}
	result := listing{
		Path:      name,
		Entries:   make([]entry, 0, len(children)),
		Snapshots: s.pack.Snapshots(),
		Container: info.Name(),
		Size:      info.Size(),
	}
	for _, child := range children {
		info, err := child.Info()
		if err != nil {
			return err
		}
		result.Entries = append(result.Entries, entry{Name: child.Name(), Directory: child.IsDir(), Size: info.Size()})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(result)
}

// download 保持流式输出；传输开始后的错误交给 HTTP 中止响应，避免把错误文字写进文件。
func (s *server) download(w http.ResponseWriter, r *http.Request) (err error) {
	source, closeSource, err := s.source(r.URL.Query().Get("snapshot"))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeSource(); err == nil {
			err = closeErr
		}
	}()
	name := r.URL.Query().Get("path")
	info, err := fs.Stat(source, name)
	if err != nil {
		return err
	}
	filename := info.Name()
	if info.IsDir() {
		if name == "." {
			filename = "root"
		}
		filename += ".zip"
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		w.WriteHeader(http.StatusOK)
		if err := transfer.Zip(w, source, name); err != nil {
			panic(http.ErrAbortHandler)
		}
		return nil
	}
	f, err := source.Open(name)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (s *server) upload(_ http.ResponseWriter, r *http.Request) error {
	return transfer.WriteFile(transfer.Container{FS: s.pack}, r.URL.Query().Get("path"), r.Body)
}

type pathRequest struct {
	Path string `json:"path"`
}

func (s *server) mkdir(_ http.ResponseWriter, r *http.Request) error {
	var request pathRequest
	if err := readJSON(r, &request); err != nil {
		return err
	}
	return s.pack.MkdirAll(request.Path)
}

func (s *server) remove(_ http.ResponseWriter, r *http.Request) error {
	var request struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := readJSON(r, &request); err != nil {
		return err
	}
	if request.Recursive {
		return s.pack.RemoveAll(request.Path)
	}
	return s.pack.Remove(request.Path)
}

func (s *server) rename(_ http.ResponseWriter, r *http.Request) error {
	var request struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := readJSON(r, &request); err != nil {
		return err
	}
	return s.pack.Rename(request.Old, request.New)
}

func snapshotName(r *http.Request) (string, error) {
	var request struct {
		Name string `json:"name"`
	}
	err := readJSON(r, &request)
	return request.Name, err
}

func (s *server) snapshotCreate(_ http.ResponseWriter, r *http.Request) error {
	name, err := snapshotName(r)
	if err != nil {
		return err
	}
	return s.pack.Snapshot(name)
}

func (s *server) snapshotDelete(_ http.ResponseWriter, r *http.Request) error {
	name, err := snapshotName(r)
	if err != nil {
		return err
	}
	return s.pack.DeleteSnapshot(name)
}

func (s *server) snapshotRestore(_ http.ResponseWriter, r *http.Request) error {
	name, err := snapshotName(r)
	if err != nil {
		return err
	}
	return s.pack.Restore(name)
}

func (s *server) compact(_ http.ResponseWriter, _ *http.Request) error { return s.pack.Compact() }
