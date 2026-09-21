// Package store 管产出目录的布局。
//
// 目录结构不能随意动，TS 那半和沙箱脚本都按这个布局读写：
//
//	data/<源>/<YYYY-MM-DD>/     采集的原始数据
//	reports/<YYYY-MM-DD>/       模型产出的报告
//	state/                      调度状态与心跳
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Store struct{ Root string }

func New(root string) *Store { return &Store{Root: root} }

func Today() string { return time.Now().Format("2006-01-02") }

func (s *Store) Data(source, date string) string {
	return filepath.Join(s.Root, "data", source, date)
}
func (s *Store) Reports(date string) string {
	return filepath.Join(s.Root, "reports", date)
}
func (s *Store) State(name string) string {
	return filepath.Join(s.Root, "state", name)
}

func (s *Store) MkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

// WriteJSON 原子写：先写临时文件再 rename。
// 进程在写一半时被杀（超时会 killpg 整个进程组）会留下半个文件，
// 而半个 JSON 下次读出来就是解析失败。
func WriteJSON(path string, v any, indent bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b []byte
	var err error
	if indent {
		b, err = json.MarshalIndent(v, "", " ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return fmt.Errorf("序列化 %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RelTo 打印用的相对路径，取不到就回绝对路径。
func (s *Store) RelTo(path string) string {
	if r, err := filepath.Rel(s.Root, path); err == nil {
		return r
	}
	return path
}
