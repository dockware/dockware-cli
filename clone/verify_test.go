package clone

import (
	"strconv"
	"strings"
	"testing"
)

func TestBatchesKeepEveryQueryOnce(t *testing.T) {
	items := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, strconv.Itoa(i))
	}
	cases := []struct {
		n, size int
		want    string
	}{
		{0, 2, ""},
		{1, 200, "0"},
		{4, 2, "0,1|2,3"},
		{5, 2, "0,1|2,3|4"},
	}
	for _, c := range cases {
		var parts []string
		for _, b := range batches(items[:c.n], c.size) {
			parts = append(parts, strings.Join(b, ","))
		}
		if got := strings.Join(parts, "|"); got != c.want {
			t.Errorf("batches(%d items, %d) = %q, want %q", c.n, c.size, got, c.want)
		}
	}
}
