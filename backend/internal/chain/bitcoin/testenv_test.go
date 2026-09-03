package bitcoin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// loadBitcoinTestEnv 在 IDE 未注入环境变量时加载本地忽略配置。
func loadBitcoinTestEnv(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("CONFIG_FILE")) != "" {
		return
	}
	if _, sourceFile, _, ok := runtime.Caller(0); ok {
		path := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "..", "deploy", "config.local.yaml"))
		if _, err := os.Stat(path); err == nil {
			t.Setenv("CONFIG_FILE", path)
			return
		}
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取测试工作目录失败：%v", err)
	}
	directory := workingDirectory
	for depth := 0; depth < 6; depth++ {
		path := filepath.Join(directory, "deploy", "config.local.yaml")
		if _, statErr := os.Stat(path); statErr == nil {
			t.Setenv("CONFIG_FILE", path)
			return
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
}
