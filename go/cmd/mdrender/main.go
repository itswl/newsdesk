// mdrender 把一个 Markdown 文件渲染到 stdout。用于验证与对比。
package main

import (
	"fmt"
	"os"

	"github.com/itswl/newsdesk2/internal/markdown"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: mdrender <文件.md>")
		os.Exit(2)
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(markdown.Render(string(b)))
}
