package main

import (
	"fmt"
	"io"
	"os"

	"github.com/itswl/newsdesk2/internal/i18n"
)

func main() {
	b, _ := io.ReadAll(os.Stdin)
	fmt.Print(i18n.ToTraditional(string(b)))
}
