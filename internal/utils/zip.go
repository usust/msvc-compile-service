package utils

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CreateZip 根据传入文件列表创建 ZIP 压缩包。
// 压缩包中的文件名只保留 basename，这样返回结果结构稳定，
// 同时不会暴露服务端临时目录路径。
func CreateZip(destination string, files []string) error {
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()

	// ZIP 内容先直接写到当前请求的临时目录中，
	// 等所有文件写完后，再由 compiler 包读取到内存中返回。
	zipWriter := zip.NewWriter(out)

	for _, filePath := range files {
		if err := addFile(zipWriter, filePath); err != nil {
			// 这里做一次尽力关闭，避免在中途失败时留下未释放的写句柄。
			_ = zipWriter.Close()
			return err
		}
	}

	// Close 会写入中央目录并完成 ZIP 封包。
	if err := zipWriter.Close(); err != nil {
		return fmt.Errorf("close zip archive: %w", err)
	}

	return out.Close()
}

// addFile 把单个文件加入 ZIP，并且只使用文件名作为压缩包内路径。
func addFile(zipWriter *zip.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return fmt.Errorf("zip header %s: %w", path, err)
	}
	// 去掉目录层级，保持压缩包内是平铺结构：
	// sample.exe、build.log、meta.json。
	header.Name = filepath.Base(path)
	header.Method = zip.Deflate

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("zip create entry %s: %w", path, err)
	}

	if _, err := io.Copy(writer, file); err != nil {
		return fmt.Errorf("zip copy %s: %w", path, err)
	}

	return nil
}
