package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/itswl/newsdesk/internal/config"
)

// findRoot 自行推导仓库根目录。
//
// 不写绝对路径：脚本和二进制都可能从任意位置被调起，写死路径换台机器就废。
// 顺序是 环境变量 → 从可执行文件往上找 → 从当前目录往上找。
func findRoot() (string, error) {
	if v := os.Getenv("NEWSDESK_ROOT"); v != "" {
		if ok, _ := isRoot(v); ok {
			return filepath.Clean(v), nil
		}
		return "", fmt.Errorf("NEWSDESK_ROOT=%s 不像仓库根目录（缺 prompts/）", v)
	}
	var starts []string
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, s := range starts {
		for d := s; ; {
			if ok, _ := isRoot(d); ok {
				return d, nil
			}
			p := filepath.Dir(d)
			if p == d {
				break
			}
			d = p
		}
	}
	return "", fmt.Errorf("找不到仓库根目录（从可执行文件和当前目录往上都没找到 prompts/）。" +
		"可用 NEWSDESK_ROOT 指定")
}

func isRoot(d string) (bool, error) {
	fi, err := os.Stat(filepath.Join(d, "prompts", "ai-news.md"))
	return err == nil && !fi.IsDir(), err
}

// loadConfig 读根目录下的 config.conf。
//
// 只有一个文件、一个解析器。配置缺失不是错误——只在本机看简报时整份配置都是可选的。
func loadConfig(root string) (*config.Config, error) {
	p := os.Getenv("NEWSDESK_CONFIG")
	if p == "" {
		p = filepath.Join(root, "config.conf")
	}
	return config.Load(p)
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}
