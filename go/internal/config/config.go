// Package config 读 config.conf。
//
// 格式是 KEY=value——沙箱脚本还要 shell source 它，换成 TOML/YAML 就没法直接
// source 了。但解析只此一份，不存在第二份语义可能漂移的实现；重复键直接
// 报错，不猜「取第一个还是最后一个」。
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
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
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{vals: map[string]string{}, Path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 %s: %w", path, err)
	}
	return Parse(path, raw)
}

// Parse 解析已经读出来的配置内容，path 只用于报错和 Config.Path。
//
// 单独拿出来给调度器用：它解析的必须正是它比对过的那份字节。分成「比一次、
// 再打开读一次」的话，两步之间落下的一次保存会让两边对不上。
func Parse(path string, raw []byte) (*Config, error) {
	c := &Config{vals: map[string]string{}, Path: path}
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
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
			// ⚠️ 只报行号，不回显这一行：少了 = 的多半是贴错地方的凭据（一截令牌、
			// 一个 webhook、折行的私钥），而这条报错会进调度器日志、会发到告警群里。
			return nil, fmt.Errorf("%s:%d 不是 KEY=value（内容不回显，这一行可能是凭据）", path, line)
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
	// DANGER 的值必须先认出来才算解析成功。这个键猜错的代价不对称：把「开着」
	// 认成「关着」只是白跑一档，把「关着」认成「开着」则等于把沙箱、网络、凭据
	// 拒读和发布前自检**一起关掉**——而这是一条无人值守、产出当天公开发布的
	// 流水线。所以不取默认值，认不出来就按坏配置处理（调度器沿用上一份并告警）。
	if v, ok := c.vals["DANGER"]; ok && v != "" {
		if _, known := parseDanger(v); !known {
			// 只报行号与键名，不回显值：这条报错会进日志、会发到告警群
			return nil, fmt.Errorf("%s:%d DANGER 只认 1 或 0（内容不回显）", path, seen["DANGER"])
		}
	}
	return c, nil
}

// parseDanger 认 DANGER 的取值，**只认 1 和 0**。第二项为 false 表示认不出来。
//
// 不收 true/on/yes 这些别名：这个开关关掉的是整套防护，只留一种写法，「文档写
// 只认 1/0、代码其实还认一串同义词」这种漂移正是这个仓库最怕的错——报错文案说
// 只认 1/0，而 on 悄悄也能用，两边就永远对不上了。写错就报错，改一个字符的事。
func parseDanger(v string) (bool, bool) {
	switch strings.TrimSpace(v) {
	case "1":
		return true, true
	case "0":
		return false, true
	}
	return false, false
}

// DangerLine 是危险模式的一句话说明。
//
// 日志、告警、status、doctor 共用同一句：分头各写一遍的话，改了一处忘另一处，
// 同一个开关在四个地方会有四种说法。
const DangerLine = "DANGER=1 危险模式：不经沙箱、可联网、凭据可读、发布前不做凭据自检"

// Danger 是否处于危险模式：不经沙箱、可联网、凭据可读、发布前不做凭据自检。
//
// 取值已在 Parse 里校验过，所以调用方不用处理错误。缺项就是关着——默认必须是
// 关着的。nil 配置（只在本机看简报、或测试里没配 Cfg）同样算关着：这个方向的
// 猜错只是防护还在，反方向才是把防护全丢。
func (c *Config) Danger() bool {
	if c == nil {
		return false
	}
	on, ok := parseDanger(c.vals["DANGER"])
	return ok && on
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

// Equal 两份配置的键值是否完全相同。路径不算：同样的内容换个地方读出来还是同一份。
func (c *Config) Equal(o *Config) bool {
	return maps.Equal(c.vals, o.vals)
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
