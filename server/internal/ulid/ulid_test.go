package ulid

import (
	"testing"
	"time"
)

func TestNewIsValidAndSortable(t *testing.T) {
	a := NewAt(time.UnixMilli(1_700_000_000_000))
	b := NewAt(time.UnixMilli(1_700_000_001_000))

	if !Valid(a) || !Valid(b) {
		t.Fatalf("生成的 ULID 应当合法: %q %q", a, b)
	}
	if len(a) != Length {
		t.Fatalf("长度应为 %d，实际 %d", Length, len(a))
	}
	if !(a < b) {
		t.Fatalf("时间更早的 ULID 应当字典序更小: %q vs %q", a, b)
	}
}

func TestNewIsUniqueWithinSameMillisecond(t *testing.T) {
	const n = 5000
	seen := make(map[string]struct{}, n)
	ts := time.UnixMilli(1_700_000_000_000)
	for i := 0; i < n; i++ {
		id := NewAt(ts)
		if _, ok := seen[id]; ok {
			t.Fatalf("同一毫秒内出现重复 ULID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTimeRoundTrip(t *testing.T) {
	want := time.UnixMilli(1_700_000_123_456).UTC()
	got, ok := Time(NewAt(want))
	if !ok {
		t.Fatal("Time 解析失败")
	}
	if !got.Equal(want) {
		t.Fatalf("时间不一致: want %s got %s", want, got)
	}
}

func TestValidRejectsBadInput(t *testing.T) {
	cases := []string{
		"",
		"01J9Z4QK7M3F8N2P5R7T9V1X3",   // 25 位
		"01J9Z4QK7M3F8N2P5R7T9V1X3BB", // 27 位
		"01J9Z4QK7M3F8N2P5R7T9V1X3I",  // 含非法字符 I
		"01J9Z4QK7M3F8N2P5R7T9V1X3l",  // 小写非法
	}
	for _, c := range cases {
		if Valid(c) {
			t.Fatalf("应当判定为非法: %q", c)
		}
	}
}

// TestGolden 校验与协议 golden 样例中的固定字符串一致。
func TestGolden(t *testing.T) {
	golden := "01J9Z4QK7M3F8N2P5R7T9V1X3B"
	if !Valid(golden) {
		t.Fatalf("golden 样例应当合法: %s", golden)
	}
	ts, ok := Time(golden)
	if !ok {
		t.Fatal("golden 解析失败")
	}
	if ts.Year() < 2020 || ts.Year() > 2100 {
		t.Fatalf("golden 时间戳异常: %s", ts)
	}
}
