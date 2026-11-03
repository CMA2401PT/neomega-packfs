// Package packfs 把目录树保存在单个容器文件内，适合需要随机覆盖写的 Minecraft 存档。
// 写时复制让快照共享未改动的块，双超级块提供提交的发布边界。
// File.Sync 会提交整个容器，实例及其句柄应由同一个 goroutine 使用。
package packfs
