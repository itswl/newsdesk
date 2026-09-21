// Package config 读 config.conf。
//
// 格式是 KEY=value——沙箱脚本还要 shell source 它，换成 TOML/YAML 就没法直接
// source 了。但解析只此一份，不存在第二份语义可能漂移的实现；重复键直接
// 报错，不猜「取第一个还是最后一个」。
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config 是一份已解析的配置。缺项返回零值，由调用方决定是必需还是可选。
type Config struct {
	vals map[string]string
	Path string
}

// Load 读指定路径。文件不存在返回空配置而非错误——只在本机看简报时
// 整个配置都是可选的，不该因此拒绝启动。
func Load(path string) (*Config, error) {
	c := &Config{vals: map[string]string{}, Path: path}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("打开 %s: %w", path, err)
	}
	defer f.Close()

	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := sc.Text()
		if i := strings.IndexByte(s, '#'); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d 不是 KEY=value: %q", path, line, s)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		// 重复键必须报错。静默取其一的话，Go 与 shell source 两个消费方的
		// 取值方向相反（第一个 vs 最后一个），排查时会看到两个组件读到不同的值。
		if prev, dup := seen[k]; dup {
			return nil, fmt.Errorf("%s:%d 重复的键 %s（已在第 %d 行出现）", path, line, k, prev)
		}
		seen[k] = line
		c.vals[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读 %s: %w", path, err)
	}
	return c, nil
}

// Get 返回字符串值，缺项返回 def。
func (c *Config) Get(key, def string) string {
	if v, ok := c.vals[key]; ok && v != "" {
		return v
	}
	return def
}

// Int 返回整数值，缺项或解析失败返回 def。
func (c *Config) Int(key string, def int) int {
	v := c.Get(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// List 按逗号切分，去空白、丢空项。
func (c *Config) List(key string) []string {
	var out []string
	for _, p := range strings.Split(c.Get(key, ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Require 一次检查多个必需项，缺哪些一并报出来——一个个报会让人来回改三遍。
func (c *Config) Require(keys ...string) error {
	var missing []string
	for _, k := range keys {
		if c.Get(k, "") == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s 缺少必需项: %s", c.Path, strings.Join(missing, "、"))
	}
	return nil
}

// All 返回全部键值的一份拷贝。给发布前的凭据自检用——它需要知道「真实值
// 长什么样」才能在产出里找它们。
func (c *Config) All() map[string]string {
	out := make(map[string]string, len(c.vals))
	for k, v := range c.vals {
		out[k] = v
	}
	return out
}
