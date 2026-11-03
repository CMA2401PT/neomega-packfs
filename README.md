# neomega-packfs

`packfs` 将目录树保存在一个容器文件中，适合打包 Minecraft 存档或大量小文件。Go 程序可以通过标准 `io/fs` 接口读取它，通过 `PackFS` 修改文件，并用提交和快照保留完整版本。

写入先改变当前实例的工作状态，`Commit()` 才将整个容器的修改持久化。快照保留某次提交的目录树，之后修改当前文件不会改变快照中的内容。模块只依赖 Go 标准库，需要 Go 1.25.5 或以上。

## 先运行一个完整示例

在本模块目录执行：

```sh
go run ./examples/basic
```

[示例源码](examples/basic/main.go) 演示创建容器、随机写入、提交、读取快照、恢复和压缩。它使用临时目录，结束后自动删除，不需要准备 Minecraft 存档。

## 调用前需要知道

- 容器实例及其文件句柄、快照视图由同一个 goroutine 使用。
- `File.Sync()` 提交整个容器，其他文件和目录的修改也一起生效。`File.Close()` 只释放句柄，`PackFS.Close()` 会提交并关闭容器。
- `Snapshot(name)` 自动提交当前状态并保存快照。`SnapshotFS(name)` 返回只读视图，用完后调用 `Close()`。
- `Restore(name)` 丢弃未提交的修改，恢复并提交快照内容。调用前必须关闭所有文件句柄；只读视图可以继续打开。
- `Compact()` 整理内部空洞并保留全部快照，调用前必须关闭所有文件句柄和只读视图。删除文件不一定立刻让容器变小，因为快照可能仍引用旧内容。

底层写入或同步失败后，当前实例会保留故障并拒绝继续修改。调用方应关闭它并处理错误，重新打开时会选择校验有效的最新已发布版本。

## 直接操作容器文件


操作系统不能把容器文件直接当成目录打开。命令行程序 `cli` 可以导入、导出和管理里面的文件。`webui` 则提供一个在浏览器中使用的文件管理页面，两种方式都不需要编写 Go 代码。下面的命令从 `neomega-packfs` 模块目录运行，需要本机安装 Go。

先创建一份示例目录，再把它装入容器：

```sh
demo_dir=$(mktemp -d)
mkdir -p "$demo_dir/source/region"
printf 'original\n' > "$demo_dir/source/notes.txt"

go run ./cmd/cli "$demo_dir/world.pack" put "$demo_dir/source" world
go run ./cmd/cli "$demo_dir/world.pack" ls world
go run ./cmd/cli "$demo_dir/world.pack" tree
go run ./cmd/cli "$demo_dir/world.pack" cat world/notes.txt
```

`ls` 和 `tree` 显示类型、字节数和名字。容器内路径使用 `/` 分隔，`.` 表示根目录。`put` 和 `get` 的目标是完整路径：源是目录时，源目录中的内容放进目标目录；源是文件时，目标就是要写入的文件名。已有目标文件会被覆盖。导出目录会保留其中的空目录。

快照可以保留覆盖之前的内容。接着运行下面的命令，可以分别导出快照里的旧文件和恢复后的当前目录：

```sh
go run ./cmd/cli "$demo_dir/world.pack" snapshot create before
printf 'replacement\n' > "$demo_dir/source/notes.txt"
go run ./cmd/cli "$demo_dir/world.pack" put "$demo_dir/source/notes.txt" world/notes.txt
go run ./cmd/cli "$demo_dir/world.pack" get -snapshot before world "$demo_dir/old"
go run ./cmd/cli "$demo_dir/world.pack" snapshot restore before
go run ./cmd/cli "$demo_dir/world.pack" get world "$demo_dir/restored"
```

`ls`、`tree`、`cat` 和 `get` 都支持 `-snapshot 名字`，选项可以放在路径前后。这些只读命令以及 `snapshot list` 遇到不存在的容器会报错。写操作可以创建容器，每条命令完成后都会提交并关闭它。命令失败时向 stderr 输出一行错误，并返回非零退出码。

其他常用操作可以这样运行：

```sh
go run ./cmd/cli "$demo_dir/world.pack" mkdir imports/empty
go run ./cmd/cli "$demo_dir/world.pack" mv imports/empty imports/renamed
go run ./cmd/cli "$demo_dir/world.pack" rm imports/renamed
go run ./cmd/cli "$demo_dir/world.pack" rm -r imports
go run ./cmd/cli "$demo_dir/world.pack" snapshot list
go run ./cmd/cli "$demo_dir/world.pack" snapshot delete before
go run ./cmd/cli "$demo_dir/world.pack" compact
go run ./cmd/cli -h
```

`rm` 删除文件或空目录，`rm -r` 递归删除整个目录。`mv` 移动或重命名文件和目录。`compact` 在回收后仍有内部空洞时重新整理容器。

如果希望在浏览器中操作，启动 Web UI：

```sh
go run ./cmd/webui -addr 127.0.0.1:8080 "$demo_dir/world.pack"
```

终端会打印访问地址，在浏览器中打开该地址即可。默认地址是 `127.0.0.1:8080`，也可以省略 `-addr`。启动时容器不存在就会创建，按 Ctrl+C 可以结束服务并关闭容器。

页面提供面包屑导航、文件大小和容器当前大小。可以多选上传文件，也可以通过“上传文件夹”保留所选文件夹及其子目录的相对路径。点击文件即可下载，目录则下载为 ZIP。页面还可以新建目录、删除、重命名和压缩容器。

“浏览版本”可以切换到快照，只读浏览和下载其中的内容。快照管理按钮用于创建、删除或恢复快照。删除目录和恢复快照都会先要求确认。每个修改请求返回前都会提交，页面资源全部内嵌在程序中。

## 在 Go 程序中使用


导入路径为 `neomega-packfs`，包名为 `packfs`。模块只使用 Go 标准库。

容器内路径遵循 `fs.ValidPath`：用斜杠分隔，不加前导斜杠，`"."` 表示根目录。每个名字最多 255 字节。根目录不能被删除或重命名。`RemoveAll()` 会递归删除子树的全部目录项和对象记录。

`OpenFile()` 使用 `os.O_RDONLY`、`os.O_WRONLY`、`os.O_RDWR`、`os.O_CREATE`、`os.O_TRUNC`、`os.O_APPEND` 和 `os.O_EXCL`。追加句柄的 `Write()` 始终写在末尾，`WriteAt()` 返回 `fs.ErrInvalid`。`Rename()` 可以覆盖已有文件，已有目录作为目标时返回 `fs.ErrExist`。目录不能移进自己的子孙，目录和文件之间也不能相互覆盖。

路径操作通过 `*fs.PathError` 包装 `fs.ErrNotExist`、`fs.ErrExist`、`fs.ErrInvalid` 等错误，调用者可以用 `errors.Is()` 判断。非空目录的 `Remove()`、有文件句柄时的 `Restore()`，以及有句柄或视图时的 `Compact()` 都返回 `fs.ErrInvalid`。

完整可运行示例在 [examples/basic/main.go](examples/basic/main.go)。它在临时目录创建容器，在偏移 8192 处写入 `chunk`，用 `File.Sync()` 提交，再创建快照并覆盖当前文件。最后从快照读取旧内容，恢复快照并压缩容器。在模块目录运行：

```sh
go run ./examples/basic
```

输出为：

```text
快照长度：8197，末尾：chunk
```

开发结构、持久化规则和验收命令见 [AGENTS.md](AGENTS.md)。
