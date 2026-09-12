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
