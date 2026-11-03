package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIEndToEnd 使用真实进程验证每条命令关闭重开后仍能读取正确版本。
func TestCLIEndToEnd(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "packfs-cli")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	call := func(args ...string) (string, string, error) {
		command := exec.Command(binary, args...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	container := filepath.Join(dir, "world.pack")
	command := func(args ...string) string {
		t.Helper()
		out, stderr, err := call(append([]string{container}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, stderr)
		}
		return out
	}
	for _, args := range [][]string{
		{"ls"},
		{"tree"},
		{"cat", "missing"},
		{"get", "missing", filepath.Join(dir, "missing")},
		{"snapshot", "list"},
	} {
		_, stderr, err := call(append([]string{container}, args...)...)
		if err == nil || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("missing container %v: err=%v stderr=%q", args, err, stderr)
		}
		if _, err := os.Stat(container); !os.IsNotExist(err) {
			t.Fatal("read command created a container")
		}
	}
	for _, args := range [][]string{nil, {"-h"}, {container, "-h"}} {
		out, stderr, err := call(args...)
		if err != nil || stderr != "" || !strings.Contains(out, "用法") {
			t.Fatalf("help: %v %q %q", err, out, stderr)
		}
	}
	source := filepath.Join(dir, "source")
	check(t, os.MkdirAll(filepath.Join(source, "region", "empty"), 0755))
	check(t, os.WriteFile(filepath.Join(source, "level.dat"), []byte("original"), 0644))
	region := bytes.Repeat([]byte("region"), 2000)
	check(t, os.WriteFile(filepath.Join(source, "region", "r.0.0.mca"), region, 0644))
	command("put", source, "world")
	out := command("ls", "world")
	if !strings.Contains(out, "file\t8\t\"level.dat\"") || !strings.Contains(out, "dir\t0\t\"region\"") {
		t.Fatalf("ls: %s", out)
	}
	command("snapshot", "create", "before")
	check(t, os.WriteFile(filepath.Join(source, "level.dat"), []byte("replacement"), 0644))
	command("put", filepath.Join(source, "level.dat"), "world/level.dat")
	old := filepath.Join(dir, "old")
	command("get", "-snapshot", "before", "world", old)
	wantFile(t, filepath.Join(old, "level.dat"), []byte("original"))
	wantFile(t, filepath.Join(old, "region", "r.0.0.mca"), region)
	if info, err := os.Stat(filepath.Join(old, "region", "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty directory was not copied")
	}
	if out := command("cat", "world/level.dat"); out != "replacement" {
		t.Fatalf("cat: %q", out)
	}
	command("snapshot", "restore", "before")
	restored := filepath.Join(dir, "restored")
	command("get", "world", restored)
	wantFile(t, filepath.Join(restored, "level.dat"), []byte("original"))
	if out := command("snapshot", "list"); out != "before\n" {
		t.Fatalf("snapshots: %q", out)
	}
	command("mkdir", "extra/deep")
	command("mv", "world/level.dat", "extra/deep/moved")
	command("compact")
	if out := command("cat", "extra/deep/moved"); out != "original" {
		t.Fatalf("mv: %q", out)
	}
	command("rm", "-r", "world")
	if out := command("tree", "world", "-snapshot", "before"); !strings.Contains(out, "r.0.0.mca") {
		t.Fatalf("snapshot tree: %s", out)
	}
	command("snapshot", "delete", "before")
	if out := command("snapshot", "list"); out != "" {
		t.Fatalf("deleted snapshot still listed: %s", out)
	}
	_, stderr, err := call(container, "cat", "missing\nname")
	if err == nil || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("error output is not one line: %q", stderr)
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantFile(t *testing.T, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(name)
	check(t, err)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: content differs", name)
	}
}
