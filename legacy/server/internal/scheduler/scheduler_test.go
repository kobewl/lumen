package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"lumen/server/internal/ai"
	"lumen/server/internal/sessions"
	"lumen/server/internal/storage"
	"lumen/server/internal/summary"
)

var testLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// newTestScheduler 搭建一个带假模型服务的调度器。
func newTestScheduler(t *testing.T, calls *int) (*Scheduler, *storage.Store, *summary.Service) {
	t.Helper()

	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)

		// 回一个合法但内容随输入变化的总结，以便观察是否重新生成。
		out := map[string]any{
			"id": "mock", "model": "deepseek-chat",
			"choices": []map[string]any{{
				"message":       map[string]any{"content": `{"date":"` + time.Now().In(testLoc).Format("2006-01-02") + `","headline":"进度","projects":[],"uncertainties":[]}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := sessions.NewEngine(store, testLoc)
	client := ai.NewClient(server.URL, "test-key", "deepseek-chat", 10*time.Second)
	svc := summary.NewService(store, engine, client, nil, testLoc, 5, nil, logger)

	sched := New(store, svc, testLoc, 22, 30, 30, t.TempDir(), 7, logger)
	return sched, store, svc
}

// insertActivity 写入一条窗口活动事件，改变输入数据。
//
// id 必须真正唯一：事件表以 id 为主键，重复 id 会被幂等逻辑丢弃，
// 导致"以为插入了新数据，实际没有"，测试就会误判。
func insertActivity(t *testing.T, store *storage.Store, at time.Time, minutes int, project string) {
	t.Helper()
	ctxJSON, _ := json.Marshal(map[string]any{"app": "ZCode", "project": project})
	dataJSON, _ := json.Marshal(map[string]any{"duration_seconds": minutes * 60})

	// 用时间戳 + 时长派生 id，保证每次调用都不同。
	id := "01J9Z4QK7M3F8N2P5R7T9" + ulidSuffix(at, minutes)
	if _, err := store.InsertEvent(context.Background(), storage.Event{
		ID: id, DeviceID: "desktop-mac-01", Type: "window.activity",
		Timestamp: at, ReceivedAt: at, PrivacyLevel: "P0",
		ContextJSON: string(ctxJSON), DataJSON: string(dataJSON), BatchID: "test",
	}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}
}

// ulidSuffix 用时间与时长构造 3 位 Crockford Base32 后缀（共 26 位 id）。
func ulidSuffix(at time.Time, minutes int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := at.Unix()/60 + int64(minutes)
	out := make([]byte, 3)
	for i := 2; i >= 0; i-- {
		out[i] = alphabet[n&0x1f]
		n >>= 5
	}
	return string(out)
}

// TestSummaryRegeneratesWhenDataChangesAfterManualRun 是回归测试。
//
// 场景：白天手工生成过一次总结（联调或补发），之后又有新的工作数据。
// 晚上 22:30 的自动总结必须用完整数据重新生成，而不是因为"已有成功总结"被跳过。
// 之前的实现只要有成功总结就 return，导致用户收到残缺的日总结。
func TestSummaryRegeneratesWhenDataChangesAfterManualRun(t *testing.T) {
	calls := 0
	sched, store, svc := newTestScheduler(t, &calls)
	ctx := context.Background()
	today := time.Now().In(testLoc)
	date := today.Format("2006-01-02")

	// 上午的数据，并手工生成一次总结（模拟联调）。
	morning := time.Date(today.Year(), today.Month(), today.Day(), 10, 0, 0, 0, testLoc)
	insertActivity(t, store, morning, 60, "lumen")
	if _, err := svc.GenerateForDate(ctx, date); err != nil {
		t.Fatalf("手工生成失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("手工生成应调用模型 1 次，实际 %d", calls)
	}

	// 下午又工作了（数据变化）。
	afternoon := time.Date(today.Year(), today.Month(), today.Day(), 15, 0, 0, 0, testLoc)
	insertActivity(t, store, afternoon, 90, "lumen")

	// 22:30 的调度触发。
	triggerAt := time.Date(today.Year(), today.Month(), today.Day(), 22, 30, 0, 0, testLoc)
	sched.trySummary(ctx, triggerAt)

	if calls != 2 {
		t.Fatalf("数据变化后应重新生成总结（调用模型 2 次），实际 %d 次", calls)
	}
}

// TestSummarySkipsWhenDataUnchanged 确认数据未变时不会重复调用模型。
func TestSummarySkipsWhenDataUnchanged(t *testing.T) {
	calls := 0
	sched, store, svc := newTestScheduler(t, &calls)
	ctx := context.Background()
	today := time.Now().In(testLoc)
	date := today.Format("2006-01-02")

	morning := time.Date(today.Year(), today.Month(), today.Day(), 10, 0, 0, 0, testLoc)
	insertActivity(t, store, morning, 60, "lumen")
	if _, err := svc.GenerateForDate(ctx, date); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("应调用 1 次，实际 %d", calls)
	}

	// 数据没变，22:30 再触发不应重复调用模型（input_hash 相同）。
	triggerAt := time.Date(today.Year(), today.Month(), today.Day(), 22, 30, 0, 0, testLoc)
	sched.trySummary(ctx, triggerAt)

	if calls != 1 {
		t.Fatalf("数据未变时不应重复调用模型，实际 %d 次", calls)
	}
}

// TestSummaryNotTriggeredBeforeTargetTime 确认未到时间不触发。
func TestSummaryNotTriggeredBeforeTargetTime(t *testing.T) {
	calls := 0
	sched, _, _ := newTestScheduler(t, &calls)
	ctx := context.Background()
	today := time.Now().In(testLoc)

	early := time.Date(today.Year(), today.Month(), today.Day(), 9, 0, 0, 0, testLoc)
	sched.trySummary(ctx, early)

	if calls != 0 {
		t.Fatalf("未到 22:30 不应触发总结，实际调用 %d 次", calls)
	}
}
