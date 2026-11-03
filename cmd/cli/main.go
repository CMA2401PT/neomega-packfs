package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	packfs "neomega-packfs"
	"neomega-packfs/cmd/internal/transfer"
)

const usage = `用法：go run ./cmd/cli <容器文件> <命令> [参数]

ls [路径]                     列出类型、字节数和名字，默认根目录 .
tree [路径]                   递归列出目录树
cat <路径>                    将文件原样输出到 stdout
mkdir <路径>                  递归创建目录
put <本机路径> <容器路径>     复制文件或目录，已有文件覆盖
get <容器路径> <本机路径>     导出文件或目录
rm [-r] <路径>                删除文件或空目录，-r 递归删除
mv <旧路径> <新路径>          移动或重命名
snapshot create <名字>        创建快照
snapshot list                 列出快照
snapshot delete <名字>        删除快照
snapshot restore <名字>       恢复快照
compact                       压缩容器

ls、tree、cat、get 支持 -snapshot <名字>，选项可放在路径前后。
复制目标是完整目标路径；复制目录时，源目录内容放入该目标目录。
容器路径使用 / 分隔，. 表示根目录；-h 打印帮助，-- 结束选项。
`

// invocation 在打开容器前确定命令和参数，避免错误命令意外创建容器。
type invocation struct {
	container   string
	command     string
	args        []string
	snapshot    string
	snapshotSet bool
	recursive   bool
	write       bool
	help        bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		message := strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(err.Error())
		fmt.Fprintln(os.Stderr, message)
		os.Exit(1)
	}
}

func parse(args []string) (invocation, error) {
	var cmd invocation
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		cmd.help = true
		return cmd, nil
	}
	if len(args) < 2 {
		return cmd, fmt.Errorf("需要容器文件和命令；使用 -h 查看帮助")
	}
	cmd.container, cmd.command = args[0], args[1]
	if cmd.command == "-h" || cmd.command == "--help" {
		cmd.help = true
		return cmd, nil
	}
	options := true
	for i := 2; i < len(args); i++ {
		arg := args[i]
		if options {
			switch arg {
			case "--":
				options = false
				continue
			case "-h", "--help":
				cmd.help = true
				return cmd, nil
			case "-snapshot":
				if i+1 == len(args) {
					return cmd, fmt.Errorf("-snapshot 后需要快照名字")
				}
				i++
				cmd.snapshot = args[i]
				cmd.snapshotSet = true
				continue
			case "-r":
				cmd.recursive = true
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return cmd, fmt.Errorf("未知选项 %q", arg)
			}
		}
		cmd.args = append(cmd.args, arg)
	}
	read := cmd.command == "ls" || cmd.command == "tree" || cmd.command == "cat" || cmd.command == "get"
	if cmd.snapshotSet && !read {
		return cmd, fmt.Errorf("%s 不支持 -snapshot", cmd.command)
	}
	if cmd.recursive && cmd.command != "rm" {
		return cmd, fmt.Errorf("只有 rm 支持 -r")
	}
	count := len(cmd.args)
	valid := false
	switch cmd.command {
	case "ls", "tree":
		valid = count <= 1
	case "cat", "mkdir", "rm":
		valid = count == 1
	case "put", "get", "mv":
		valid = count == 2
	case "compact":
		valid = count == 0
	case "snapshot":
		if count == 1 && cmd.args[0] == "list" {
			valid = true
		} else if count == 2 {
			valid = cmd.args[0] == "create" || cmd.args[0] == "delete" || cmd.args[0] == "restore"
		}
	default:
		return cmd, fmt.Errorf("未知命令 %q；使用 -h 查看帮助", cmd.command)
	}
	if !valid {
		return cmd, fmt.Errorf("%s 参数不正确；使用 -h 查看帮助", cmd.command)
	}
	cmd.write = !read && !(cmd.command == "snapshot" && cmd.args[0] == "list")
	return cmd, nil
}

// run 让视图和复制句柄先关闭，再由容器 Close 完成提交并报告错误。
func run(args []string, out io.Writer) (err error) {
	cmd, err := parse(args)
	if err != nil {
		return err
	}
	if cmd.help {
		_, err := io.WriteString(out, usage)
		return err
	}
	if !cmd.write {
		if _, err := os.Stat(cmd.container); err != nil {
			return err
		}
	}
	p, err := packfs.Open(cmd.container)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := p.Close(); err == nil {
			err = closeErr
		}
	}()
	var source fs.FS = p
	if cmd.snapshotSet {
		view, openErr := p.SnapshotFS(cmd.snapshot)
		if openErr != nil {
			return openErr
		}
		defer func() {
			if closeErr := view.Close(); err == nil {
				err = closeErr
			}
		}()
		source = view
	}
	return execute(
		p,
		source,
		cmd,
		out,
	)
}

func execute(p *packfs.PackFS, source fs.FS, cmd invocation, out io.Writer) error {
	switch cmd.command {
	case "ls", "tree":
		name := "."
		if len(cmd.args) != 0 {
			name = cmd.args[0]
		}
		return list(
			source,
			name,
			cmd.command == "tree",
			out,
		)
	case "cat":
		return cat(source, cmd.args[0], out)
	case "mkdir":
		return p.MkdirAll(cmd.args[0])
	case "put":
		return transfer.Import(cmd.args[0], p, cmd.args[1])
	case "get":
		return transfer.Copy(
			source,
			cmd.args[0],
			transfer.Local{},
			cmd.args[1],
		)
	case "rm":
		if cmd.recursive {
			return p.RemoveAll(cmd.args[0])
		}
		return p.Remove(cmd.args[0])
	case "mv":
		return p.Rename(cmd.args[0], cmd.args[1])
	case "compact":
		return p.Compact()
	case "snapshot":
		switch cmd.args[0] {
		case "list":
			for _, name := range p.Snapshots() {
				if _, err := fmt.Fprintln(out, name); err != nil {
					return err
				}
			}
			return nil
		case "create":
			return p.Snapshot(cmd.args[1])
		case "delete":
			return p.DeleteSnapshot(cmd.args[1])
		case "restore":
			return p.Restore(cmd.args[1])
		}
	}
	return nil
}

func cat(source fs.FS, name string, out io.Writer) (err error) {
	f, err := source.Open(name)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	_, err = io.Copy(out, f)
	return err
}

func list(source fs.FS, name string, recursive bool, out io.Writer) error {
	if recursive {
		return fs.WalkDir(source, name, func(current string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			depth := 0
			if current != name {
				relative := current
				if name != "." {
					relative = strings.TrimPrefix(current, name+"/")
				}
				depth = strings.Count(relative, "/") + 1
			}
			return printEntry(
				out,
				strings.Repeat("  ", depth),
				current,
				info,
			)
		})
	}
	info, err := fs.Stat(source, name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return printEntry(
			out,
			"",
			name,
			info,
		)
	}
	entries, err := fs.ReadDir(source, name)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := printEntry(
			out,
			"",
			entry.Name(),
			info,
		); err != nil {
			return err
		}
	}
	return nil
}

func printEntry(out io.Writer, indent, name string, info fs.FileInfo) error {
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	_, err := fmt.Fprintf(
		out,
		"%s%s\t%d\t%q\n",
		indent,
		kind,
		info.Size(),
		name,
	)
	return err
}
