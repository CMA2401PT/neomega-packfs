// 本示例在临时目录中演示随机写入、提交、快照、恢复与压缩。
// 在 neomega-packfs 模块目录运行：go run ./examples/basic。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	packfs "neomega-packfs"
)

func main() {
	// 1. 临时目录只用于让示例可重复运行；实际应用改用自己的容器路径。
	var directory string
	var err error
	directory, err = os.MkdirTemp("", "packfs-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(directory)

	var container *packfs.PackFS
	container, err = packfs.Open(filepath.Join(directory, "world.pack"))
	if err != nil {
		panic(err)
	}
	// Close 会提交整个容器；先关闭容器，随后上面的 defer 才删除临时目录。
	defer func() {
		if err := container.Close(); err != nil {
			panic(err)
		}
	}()

	// 2. 容器内使用相对路径。这里借用 MCA 文件名演示随机写，不构造有效的存档。
	if err := container.MkdirAll("region"); err != nil {
		panic(err)
	}
	const regionPath string = "region/r.0.0.mca"
	var region *packfs.File
	region, err = container.Create(regionPath)
	if err != nil {
		panic(err)
	}
	// MCA 头部占 8 KiB，所以演示从此偏移写入；前面的空洞读出来全是零。
	const payloadOffset int64 = 8 * 1024
	if _, err := region.WriteAt([]byte("chunk"), payloadOffset); err != nil {
		panic(err)
	}
	// File.Sync 提交整个容器，包括上面新建的目录，不只是当前文件。
	if err := region.Sync(); err != nil {
		panic(err)
	}
	// Restore 要求没有打开的文件句柄，因此这里立即关闭；Close 本身不提交。
	if err := region.Close(); err != nil {
		panic(err)
	}

	// 3. Snapshot 会自动提交并保留当前版本；后续 WriteFile 会截断并覆盖当前文件。
	const snapshotName string = "before-update"
	if err := container.Snapshot(snapshotName); err != nil {
		panic(err)
	}
	if err := container.WriteFile(regionPath, []byte("new")); err != nil {
		panic(err)
	}
	if err := container.Commit(); err != nil {
		panic(err)
	}

	// 4. 快照视图仍能读取覆盖之前的内容，ReadFile 返回的字节可在视图关闭后使用。
	var snapshot *packfs.View
	snapshot, err = container.SnapshotFS(snapshotName)
	if err != nil {
		panic(err)
	}
	var original []byte
	original, err = snapshot.ReadFile(regionPath)
	if err != nil {
		panic(err)
	}
	// Compact 要求视图也全部关闭；Restore 只要求文件句柄关闭。
	if err := snapshot.Close(); err != nil {
		panic(err)
	}
	fmt.Printf("快照长度：%d，末尾：%s\n", len(original), original[payloadOffset:])

	// 5. Restore 丢弃未提交修改并提交快照根；Compact 整理空洞，仍保留快照名字。
	if err := container.Restore(snapshotName); err != nil {
		panic(err)
	}
	if err := container.Compact(); err != nil {
		panic(err)
	}
}
