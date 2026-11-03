package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
)

const usage = "用法：go run ./cmd/webui [-addr 127.0.0.1:8080] <容器文件>\n"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(err.Error()))
		os.Exit(1)
	}
}

// run 在 HTTP 请求全部结束后关闭容器，退出时不会与下载或写入争用实例。
func run(ctx context.Context, args []string, out io.Writer) (err error) {
	flags := flag.NewFlagSet("webui", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	addr := flags.String("addr", "127.0.0.1:8080", "监听地址")
	if parseErr := flags.Parse(args); parseErr != nil {
		if parseErr == flag.ErrHelp {
			_, err = io.WriteString(out, usage)
			return err
		}
		return parseErr
	}
	if flags.NArg() == 0 {
		_, err = io.WriteString(out, usage)
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("需要一个容器文件；使用 -h 查看帮助")
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	app, err := openServer(
		flags.Arg(0),
		listener.Addr().String(),
	)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := app.close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := fmt.Fprintf(out, "访问地址：http://%s\n", listener.Addr()); err != nil {
		return err
	}
	server := &http.Server{Handler: app.handler()}
	finished := make(chan struct{})
	defer close(finished)
	shutdown := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdown <- server.Shutdown(context.Background())
		case <-finished:
		}
	}()
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return <-shutdown
	}
	return err
}
