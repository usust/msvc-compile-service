package main

import (
	"log"

	"windows-compile-service/internal/host"
)

// main 只负责把进程交给宿主层启动。
func main() {
	if err := host.Run(); err != nil {
		log.Fatalf("server exited with error: %v", err)
	}
}
