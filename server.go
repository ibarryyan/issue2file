package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// serveOutput 在本地启动一个只读 HTTP 服务，用于浏览输出目录中的面板与 Markdown 文件。
// 它会一直阻塞，直到收到 SIGINT / SIGTERM 后优雅退出。
func serveOutput(dir string, port int) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("解析输出目录失败: %w", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("输出目录不可用: %s", root)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(root)))
	mux.HandleFunc("/api/issues.json", func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(root, "issues.json")
		if _, err := os.Stat(p); err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"issues.json 不存在，请先运行一次导出（确保 -dashboard 已开启）"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		http.ServeFile(w, r, p)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败（可换一个 -port）: %w", addr, err)
	}

	// 优雅退出：单独一个 goroutine 等待信号，主线程负责 Serve
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	fmt.Printf("\n面板已启动： http://%s/dashboard.html\n", addr)
	fmt.Printf("数据集接口： http://%s/api/issues.json\n", addr)
	fmt.Printf("服务目录：   %s\n", root)
	fmt.Println("按 Ctrl+C 退出")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		log.Infof("收到信号 %v，正在关闭服务...", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("关闭服务失败: %w", err)
	}
	return nil
}
