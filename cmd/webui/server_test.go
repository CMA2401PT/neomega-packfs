package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	packfs "neomega-packfs"
)

func testServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	app, err := openServer(
		filepath.Join(t.TempDir(), "world.pack"),
		"127.0.0.1:8080",
	)
	check(t, err)
	t.Cleanup(func() { check(t, app.close()) })
	return app, app.handler()
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func call(handler http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("X-PackFS", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func wantStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d, want %d: %s", response.Code, status, response.Body)
	}
}

func TestWriteRequiresHeader(t *testing.T) {
	app, handler := testServer(t)
	for _, header := range []string{"", "0", "1"} {
		t.Run("header="+header, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/mkdir",
				bytes.NewBufferString(`{"path":"pwned","x":"="}`),
			)
			request.Host = "127.0.0.1:8080"
			request.Header.Set("Origin", "https://evil.example")
			request.Header.Set("Content-Type", "text/plain")
			if header != "" {
				request.Header.Set("X-PackFS", header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if header == "1" {
				wantStatus(t, response, http.StatusNoContent)
				_, err := app.pack.Stat("pwned")
				check(t, err)
				return
			}
			wantStatus(t, response, http.StatusForbidden)
			if _, err := app.pack.Stat("pwned"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("rejected request changed container: %v", err)
			}
		})
	}
}

func TestAPIHost(t *testing.T) {
	app, err := openServer(
		filepath.Join(t.TempDir(), "world.pack"),
		"192.0.2.10:8080",
	)
	check(t, err)
	t.Cleanup(func() { check(t, app.close()) })
	handler := app.handler()
	for _, tc := range []struct {
		host   string
		status int
	}{
		{"192.0.2.10:8080", http.StatusOK},
		{"localhost:8080", http.StatusOK},
		{"127.0.0.1:8080", http.StatusOK},
		{"[::1]:8080", http.StatusOK},
		{"evil.example:8080", http.StatusForbidden},
		{"localhost:8081", http.StatusForbidden},
		{"127.0.0.1:8081", http.StatusForbidden},
		{"[::1]:8081", http.StatusForbidden},
		{"localhost", http.StatusForbidden},
	} {
		t.Run(tc.host, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				target := "/api/list"
				var body []byte
				want := tc.status
				if method == http.MethodPost {
					target = "/api/mkdir"
					body = []byte(`{"path":"allowed"}`)
					if want == http.StatusOK {
						want = http.StatusNoContent
					}
				}
				request := httptest.NewRequest(method, target, bytes.NewReader(body))
				request.Host = tc.host
				request.Header.Set("X-PackFS", "1")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				wantStatus(t, response, want)
				if method == http.MethodPost && want == http.StatusNoContent {
					check(t, app.pack.Remove("allowed"))
				}
			}
		})
	}
}

func TestWebOperationsAndSnapshots(t *testing.T) {
	app, handler := testServer(t)
	post := func(route string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		check(t, err)
		wantStatus(t, call(handler, "POST", route, body), http.StatusNoContent)
	}
	name := "folder/中文 \"雪\".txt"
	upload := "/api/upload?path=" + url.QueryEscape(name)
	wantStatus(t, call(handler, "POST", upload, []byte("original")), http.StatusNoContent)
	post("/api/mkdir", map[string]string{"path": "folder/empty"})
	response := call(handler, "GET", "/api/list?path=folder", nil)
	wantStatus(t, response, http.StatusOK)
	var listed listing
	check(t, json.Unmarshal(response.Body.Bytes(), &listed))
	if listed.Path != "folder" || listed.Size <= 0 || len(listed.Entries) != 2 {
		t.Fatalf("listing: %+v", listed)
	}
	if listed.Entries[0].Name != "empty" || !listed.Entries[0].Directory {
		t.Fatalf("directory entry: %+v", listed.Entries)
	}
	post("/api/snapshot/create", map[string]string{"name": "before"})
	wantStatus(t, call(handler, "POST", upload, []byte("replacement")), http.StatusNoContent)
	download := "/api/download?path=" + url.QueryEscape(name)
	response = call(handler, "GET", download+"&snapshot=before", nil)
	wantStatus(t, response, http.StatusOK)
	if response.Body.String() != "original" {
		t.Fatalf("snapshot download: %s", response.Body)
	}
	kind, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	check(t, err)
	if kind != "attachment" || params["filename"] != "中文 \"雪\".txt" {
		t.Fatalf("download filename: %q", response.Header().Get("Content-Disposition"))
	}
	response = call(handler, "GET", download, nil)
	wantStatus(t, response, http.StatusOK)
	if response.Body.String() != "replacement" {
		t.Fatal("current download did not reflect upload")
	}
	response = call(handler, "GET", "/api/download?path=folder&snapshot=before", nil)
	wantStatus(t, response, http.StatusOK)
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	check(t, err)
	zipped, err := archive.Open(name)
	check(t, err)
	content, err := io.ReadAll(zipped)
	check(t, err)
	check(t, zipped.Close())
	if string(content) != "original" {
		t.Fatal("directory zip lost snapshot contents")
	}
	if info, err := zipDirectory(archive, "folder/empty"); err != nil || !info {
		t.Fatal("directory zip lost empty directory")
	}
	response = call(handler, "POST", upload+"&snapshot=before", []byte("forbidden"))
	wantStatus(t, response, http.StatusForbidden)
	response = call(handler, "POST", "/api/upload?path=folder%2F..%2Fescape", []byte("invalid"))
	wantStatus(t, response, http.StatusBadRequest)
	post("/api/rename", map[string]string{"old": name, "new": "folder/renamed.txt"})
	response = call(handler, "POST", "/api/remove", []byte(`{"path":"folder"}`))
	wantStatus(t, response, http.StatusBadRequest)
	post("/api/remove", map[string]any{"path": "folder", "recursive": true})
	wantStatus(t, call(handler, "GET", "/api/list?path=folder", nil), http.StatusNotFound)
	post("/api/snapshot/restore", map[string]string{"name": "before"})
	response = call(handler, "GET", download, nil)
	wantStatus(t, response, http.StatusOK)
	if response.Body.String() != "original" {
		t.Fatal("snapshot restore did not replace current contents")
	}
	post("/api/compact", map[string]string{})
	post("/api/snapshot/delete", map[string]string{"name": "before"})
	response = call(handler, "GET", "/api/list", nil)
	wantStatus(t, response, http.StatusOK)
	check(t, json.Unmarshal(response.Body.Bytes(), &listed))
	if len(listed.Snapshots) != 0 {
		t.Fatal("deleted snapshot is still listed")
	}
	check(t, app.close())
	p, err := packfs.Open(app.filename)
	check(t, err)
	defer p.Close()
	data, err := p.ReadFile(name)
	check(t, err)
	if string(data) != "original" {
		t.Fatal("requests were not committed before returning")
	}
}

func zipDirectory(archive *zip.Reader, name string) (bool, error) {
	file, err := archive.Open(name)
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// TestConcurrentRequests 并发执行 HTTP 请求，供竞态检查验证请求锁的覆盖范围。
func TestConcurrentRequests(t *testing.T) {
	_, handler := testServer(t)
	var workers sync.WaitGroup
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			name := fmt.Sprintf("parallel/%d", index)
			data := fmt.Sprintf("content-%d", index)
			response := call(handler, "POST", "/api/upload?path="+url.QueryEscape(name), []byte(data))
			if response.Code != http.StatusNoContent {
				failures <- fmt.Errorf("upload status %d", response.Code)
				return
			}
			response = call(handler, "GET", "/api/download?path="+url.QueryEscape(name), nil)
			if response.Code != http.StatusOK || response.Body.String() != data {
				failures <- fmt.Errorf("download %s: status=%d body=%q", name, response.Code, response.Body)
			}
		}(i)
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func TestEmbeddedPage(t *testing.T) {
	_, handler := testServer(t)
	for _, name := range []string{"/", "/app.js", "/style.css"} {
		response := call(handler, "GET", name, nil)
		wantStatus(t, response, http.StatusOK)
		if response.Body.Len() == 0 {
			t.Fatalf("empty embedded asset: %s", name)
		}
	}
}
