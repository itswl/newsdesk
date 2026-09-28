package history

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/itswl/newsdesk/internal/store"
)

// 抓取失败与「抓到了但窗内没新条目」必须是两个名单。混在一起的话模型会把
// 一个只是几天没发东西的一手源写成「采集失败」——实测就这么写过。
func TestSourceGapsSplitFailedFromQuiet(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	write := func(day, content string) {
		d := filepath.Join(root, "data", "ainews", day)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "items.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-09-20", `{"items":[],"sources":[["A","FAIL",0,0],["B","OK",12,0],["C","OK",10,3]]}`)
	write("2026-09-21", `{"items":[],"sources":[["A","FAIL",0,0],["B","OK",12,0],["C","OK",10,0]]}`)
	write("2026-09-22", `{"items":[],"sources":[]}`)

	out, err := ainews(st, "2026-09-22", filepath.Join(root, "data", "ainews", "2026-09-22"))
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := out["sources_failed"].(map[string][]string)
	quiet, _ := out["sources_quiet"].(map[string][]string)
	// daysBefore 由近到远
	if want := map[string][]string{"A": {"2026-09-21", "2026-09-20"}}; !reflect.DeepEqual(failed, want) {
		t.Errorf("sources_failed = %v，想要 %v", failed, want)
	}
	if want := map[string][]string{"B": {"2026-09-21", "2026-09-20"}, "C": {"2026-09-21"}}; !reflect.DeepEqual(quiet, want) {
		t.Errorf("sources_quiet = %v，想要 %v", quiet, want)
	}
	if _, ok := out["sources_with_gaps"]; ok {
		t.Error("旧的混合名单不该再输出")
	}
}
