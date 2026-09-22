package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

func LoadDotEnv(startDirectory string) (string, error) {
	directory, err := filepath.Abs(startDirectory)
	if err != nil {
		return "", fmt.Errorf("resolve .env search directory: %w", err)
	}

	for {
		path := filepath.Join(directory, ".env")
		info, statErr := os.Stat(path)
		switch {
		case statErr == nil:
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("load %s: not a regular file", path)
			}
			if err := godotenv.Load(path); err != nil {
				return "", fmt.Errorf("load %s: %w", path, err)
			}
			return path, nil
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", fmt.Errorf("inspect %s: %w", path, statErr)
		}

		parent := filepath.Dir(directory)
		if parent == directory {
			return "", nil
		}
		directory = parent
	}
}

// LoadBeside 只加载 configPath 同目录的 .env（不做向上搜索——与配置定位的
// 显式模式一致，密钥引用的变量来源可预期）。不覆盖已有的进程环境变量；
// 文件不存在返回空串（密钥引用回落进程环境），畸形文件报清晰错误。
func LoadBeside(configPath string) (string, error) {
	path := filepath.Join(filepath.Dir(configPath), ".env")
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("load %s: not a regular file", path)
	}
	if err := godotenv.Load(path); err != nil {
		return "", fmt.Errorf("load %s: %w", path, err)
	}
	return path, nil
}
