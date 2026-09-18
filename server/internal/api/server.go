// Package api 提供 HTTP 接口。
//
// 接口清单（与产品文档一致）：
//
//	POST /api/v1/devices/register              一次性 enrollment token 注册
//	POST /api/v1/events/batch                  Bearer device token，逐事件 ACK
//	GET  /api/v1/sessions?date=YYYY-MM-DD      Bearer device token
//	GET  /api/v1/summaries/daily?date=...      Bearer device token
//	POST /api/v1/summaries/daily/generate      管理令牌
//	GET  /api/v1/healthz                       无鉴权，只返回非敏感状态
//
// V0.1 不提供开放域聊天、Memory 或管理后台。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lumen/server/internal/auth"
	"lumen/server/internal/config"
	"lumen/server/internal/events"
	"lumen/server/internal/feishu"
	"lumen/server/internal/sessions"
	"lumen/server/internal/storage"
	"lumen/server/internal/summary"
)

// Version 是当前服务端版本号。
//
// 它是变量而不是常量：构建镜像时由 deploy/Dockerfile 通过
// -ldflags "-X lumen/server/internal/api.Version=<镜像标签>" 注入。
// 这样 /api/v1/healthz 报告的就是线上实际运行的版本，
// 部署后不必靠"某个新接口在不在"来猜版本号。
// 本地 go run 未注入时显示 dev。
var Version = "dev"

// Server 组合所有 HTTP 处理依赖。
type Server struct {
	cfg     *config.Config
	store   *storage.Store
	auth    *auth.Service
	engine  *sessions.Engine
	summary *summary.Service
	qa      *feishu.QAService
	logger  *slog.Logger
}

// NewServer 创建 HTTP 服务。
// qaService 用于只读调试接口 /api/v1/ask；可以为 nil（该接口将返回未启用）。
func NewServer(cfg *config.Config, store *storage.Store, authService *auth.Service,
	engine *sessions.Engine, summaryService *summary.Service, qaService *feishu.QAService,
	logger *slog.Logger) *Server {
	return &Server{
		cfg: cfg, store: store, auth: authService, engine: engine,
		summary: summaryService, qa: qaService, logger: logger,
	}
}

// Handler 返回注册好路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/healthz", s.handleHealthz)
	mux.HandleFunc("/api/v1/devices/register", s.handleRegister)
	mux.HandleFunc("/api/v1/events/batch", s.handleBatch)
	mux.HandleFunc("/api/v1/sessions", s.handleSessions)
	mux.HandleFunc("/api/v1/sessions/rebuild", s.handleRebuildSessions)
	mux.HandleFunc("/api/v1/task-summaries", s.handleTaskSummaries)
	mux.HandleFunc("/api/v1/summaries/daily", s.handleSummary)
	mux.HandleFunc("/api/v1/summaries/daily/generate", s.handleGenerateSummary)
	// 只读调试接口：用与飞书问答相同的逻辑处理一句话，便于在不打开飞书的情况下
	// 验证问答链路与排查问题。需要管理令牌。
	mux.HandleFunc("/api/v1/ask", s.handleAsk)
	return s.withRecovery(s.withLogging(mux))
}

// ---- 中间件 ----

// withLogging 记录访问日志。只记录方法、路径、状态码和耗时，不记录请求正文。
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"elapsed_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("请求处理 panic", "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// ---- 健康检查 ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 GET")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	out := map[string]any{
		"version":      Version,
		"time":         storage.NowISO(),
		"db":           "ok",
		"ai_enabled":   s.cfg.AIEnabled(),
		"feishu_ready": s.cfg.FeishuEnabled,
	}

	if err := s.store.DB.PingContext(ctx); err != nil {
		out["db"] = "error"
	}
	if last, ok, err := s.store.LastEventReceivedAt(ctx); err == nil {
		if ok {
			out["last_event_at"] = storage.FormatISO(last)
		} else {
			out["last_event_at"] = nil
		}
	}
	if status, date, updatedAt, ok, err := s.store.LastSummaryStatus(ctx); err == nil && ok {
		out["last_summary"] = map[string]any{
			"status": status, "date": date, "updated_at": storage.FormatISO(updatedAt),
		}
	} else {
		out["last_summary"] = nil
	}
	if stats, err := s.store.TableStats(ctx); err == nil {
		out["counts"] = stats
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- 设备注册 ----

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 POST")
		return
	}
	var req struct {
		EnrollmentToken string `json:"enrollment_token"`
		DeviceName      string `json:"device_name"`
	}
	if err := decodeJSON(r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	result, err := s.auth.Register(ctx, req.EnrollmentToken, req.DeviceName)
	switch {
	case errors.Is(err, auth.ErrEnrollmentDisabled):
		writeError(w, http.StatusForbidden, "enrollment_disabled", "注册接口未开启")
		return
	case errors.Is(err, auth.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", "enrollment token 无效")
		return
	case errors.Is(err, auth.ErrDeviceLimit):
		writeError(w, http.StatusConflict, "device_limit", "已达设备上限")
		return
	case err != nil:
		s.logger.Error("设备注册失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "注册失败")
		return
	}

	s.logger.Info("设备注册成功", "device_id", result.DeviceID)
	// device_token 只在这一次响应中出现，服务端只保存哈希。
	writeJSON(w, http.StatusOK, map[string]string{
		"device_id":    result.DeviceID,
		"device_token": result.DeviceToken,
	})
}

// ---- 批量事件 ----

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 POST")
		return
	}

	device, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	// 限制请求体大小（压缩前后都限制），防止超大请求打满内存。
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxBatchBytes)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := newGzipReader(body, s.cfg.MaxBatchBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_gzip", "gzip 解压失败")
			return
		}
		defer gz.Close()
		body = gz
	}

	raw, err := io.ReadAll(io.LimitReader(body, s.cfg.MaxBatchBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "batch_too_large", "请求体超过大小上限")
		return
	}

	var req struct {
		DeviceID string            `json:"device_id"`
		BatchID  string            `json:"batch_id"`
		SentAt   string            `json:"sent_at"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求不是合法 JSON")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	serverTime := time.Now().UTC()
	results := make([]map[string]any, 0, len(req.Events))

	if req.DeviceID != "" && req.DeviceID != device.ID {
		// token 与 payload 声明的设备不一致，整批拒绝。
		_ = s.store.RecordSecurityEvent(ctx, "device_mismatch", "batch device_id 与凭证不一致", device.ID)
		writeError(w, http.StatusForbidden, "device_mismatch", "device_id 与凭证不一致")
		return
	}
	if len(req.Events) > s.cfg.MaxBatchEvents {
		writeError(w, http.StatusRequestEntityTooLarge, "too_many_events", "单批事件数超过上限")
		return
	}

	var accepted, duplicate, rejected int
	for _, rawEvent := range req.Events {
		result := s.processEvent(ctx, rawEvent, device.ID, req.BatchID, serverTime)
		switch result["status"] {
		case "accepted":
			accepted++
		case "duplicate":
			duplicate++
		default:
			rejected++
		}
		results = append(results, result)
	}

	if err := s.store.TouchDevice(ctx, device.ID); err != nil {
		s.logger.Warn("更新设备活跃时间失败", "error", err.Error())
	}

	// 有新事件入库时重算受影响日期的 Session。
	if accepted > 0 {
		s.rebuildAffectedDays(ctx, req.Events, device.ID)
	}

	s.logger.Info("批量入库完成", "device_id", device.ID, "batch_id", req.BatchID,
		"accepted", accepted, "duplicate", duplicate, "rejected", rejected)

	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id":    req.BatchID,
		"server_time": storage.FormatISO(serverTime),
		"results":     results,
	})
}

// processEvent 校验并写入单条事件，返回逐事件结果。
// 单条失败不影响整批其他事件。
func (s *Server) processEvent(ctx context.Context, raw []byte, deviceID, batchID string, now time.Time) map[string]any {
	rawEvent, verr := events.UnmarshalEvent(raw, s.cfg.MaxEventBytes)
	if verr != nil {
		return rejectedResult("", verr)
	}

	validated, clockSkew, verr := events.Validate(rawEvent, deviceID, now, s.cfg.ClockSkewTolerance)
	if verr != nil {
		// 禁用字段属于安全事件，单独记录以便追溯。
		if verr.Code == "forbidden_field" || verr.Code == "sensitive_value" {
			_ = s.store.RecordSecurityEvent(ctx, "event_rejected", verr.Message, "")
		}
		return rejectedResult(rawEvent.ID, verr)
	}

	// 客户端时间超前过多时，标记 skew 并使用 received_at 参与 Session 聚合。
	ts := validated.Timestamp
	if clockSkew {
		ts = now
	}

	res, err := s.store.InsertEvent(ctx, storage.Event{
		ID:           validated.ID,
		DeviceID:     validated.DeviceID,
		Type:         validated.Type,
		Timestamp:    ts,
		ReceivedAt:   now,
		PrivacyLevel: validated.Privacy,
		ContextJSON:  events.MarshalContext(validated.Context),
		DataJSON:     events.MarshalData(validated.Data),
		BatchID:      batchID,
		ClockSkew:    clockSkew,
	})
	if err != nil {
		s.logger.Error("事件写入失败", "event_id", validated.ID, "error", err.Error())
		out := rejectedResult(validated.ID, &events.ValidationError{Code: "storage_error", Message: "写入失败"})
		out["retryable"] = true
		return out
	}

	// 任务摘要额外投影到 agent_task_summaries。
	//
	// 原始事件已经写进 events 表（审计链完整），这里是给查询用的投影：
	// 同一个 task_id 重复汇报时覆盖旧内容，而不是在事件流里堆多份。
	// 投影失败不阻塞事件入库——事件已经落盘，用户可以靠重算补齐投影，
	// 但反过来（丢掉事件）是不可恢复的。
	if validated.Type == events.TypeAgentTaskSummary {
		if err := s.projectTaskSummary(ctx, validated, ts); err != nil {
			s.logger.Error("任务摘要投影失败", "event_id", validated.ID, "error", err.Error())
			out := rejectedResult(validated.ID,
				&events.ValidationError{Code: "storage_error", Message: "任务摘要写入失败"})
			out["retryable"] = true
			return out
		}
	}

	out := map[string]any{"event_id": validated.ID, "status": res.Status}
	if clockSkew {
		out["clock_skew"] = true
	}
	return out
}

// projectTaskSummary 把一条校验通过的任务摘要事件投影到查询表。
//
// 只取已知字段，不做透传：即使将来协议加了字段，也不会因为这里忘记过滤
// 而把新内容带进查询表。
func (s *Server) projectTaskSummary(ctx context.Context, e events.Event, ts time.Time) error {
	str := func(key string) string {
		v, _ := e.Data[key].(string)
		return v
	}
	items := func(key string) []string {
		raw, ok := e.Data[key].([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}

	_, err := s.store.UpsertTaskSummary(ctx, storage.TaskSummary{
		ID:              e.ID,
		DeviceID:        e.DeviceID,
		TaskID:          str("task_id"),
		Project:         stringFromContext(e.Context, "project"),
		App:             stringFromContext(e.Context, "app"),
		Title:           str("title"),
		Status:          str("status"),
		Outcomes:        items("outcomes"),
		OpenLoops:       items("open_loops"),
		SourceAgent:     str("source_agent"),
		SourceSessionID: str("source_session_id"),
		OccurredAt:      ts,
		UpdatedAt:       time.Now().UTC(),
	})
	return err
}

// stringFromContext 读取 context 里的字符串字段，缺失时返回空串。
func stringFromContext(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// rebuildAffectedDays 重算新事件影响到的日期。V0.1 按天粒度重算，代价可接受。
func (s *Server) rebuildAffectedDays(ctx context.Context, rawEvents []json.RawMessage, deviceID string) {
	days := map[string]time.Time{}
	for _, raw := range rawEvents {
		rawEvent, verr := events.UnmarshalEvent(raw, s.cfg.MaxEventBytes)
		if verr != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, rawEvent.Timestamp)
		if err != nil {
			continue
		}
		local := ts.In(s.cfg.Location())
		key := local.Format("2006-01-02")
		days[key] = local
	}
	for _, day := range days {
		if _, err := s.engine.RebuildDay(ctx, day); err != nil {
			s.logger.Error("重建 Session 失败", "date", day.Format("2006-01-02"), "error", err.Error())
		}
	}
}

func rejectedResult(eventID string, verr *events.ValidationError) map[string]any {
	out := map[string]any{
		"event_id": eventID,
		"status":   "rejected",
		"code":     verr.Code,
		"message":  verr.Message,
	}
	return out
}

// ---- 查询接口 ----

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 GET")
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().In(s.cfg.Location()).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, s.cfg.Location()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", "date 格式必须是 YYYY-MM-DD")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	list, err := s.store.SessionsByDate(ctx, date)
	if err != nil {
		s.logger.Error("查询 Session 失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"date":     date,
		"count":    len(list),
		"sessions": renderSessions(list, s.cfg.Location()),
	})
}

// handleTaskSummaries 返回专业 Agent 汇报的任务摘要。
//
// 参数：date=YYYY-MM-DD（默认今天）或 project=<名称>；limit 限制条数。
// 与 /api/v1/sessions 一样用 device token 鉴权：都是"这台设备自己的记录"。
//
// source_session_id 会出现在响应里（用于追溯），但**不会**进入模型上下文
// 或用户可见文本——那是 assistant 层负责过滤的事。
func (s *Server) handleTaskSummaries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 GET")
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	loc := s.cfg.Location()
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}

	var list []storage.TaskSummary
	var err error
	var scope map[string]any

	if project := r.URL.Query().Get("project"); project != "" {
		list, err = s.store.TaskSummariesByProject(ctx, project, limit)
		scope = map[string]any{"project": project}
	} else {
		date := r.URL.Query().Get("date")
		if date == "" {
			date = time.Now().In(loc).Format("2006-01-02")
		}
		day, perr := time.ParseInLocation("2006-01-02", date, loc)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "invalid_date", "date 格式必须是 YYYY-MM-DD")
			return
		}
		list, err = s.store.TaskSummariesBetween(ctx, day, day.AddDate(0, 0, 1), limit)
		scope = map[string]any{"date": date}
	}
	if err != nil {
		s.logger.Error("查询任务摘要失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "查询失败")
		return
	}

	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, renderTaskSummary(t, loc))
	}
	resp := map[string]any{"count": len(out), "task_summaries": out}
	for k, v := range scope {
		resp[k] = v
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRebuildSessions 重算指定日期范围的 Session。
//
// 存在的意义：Session 规则修好后（例如修正 idle 切断方向、忽略锁屏期间的
// 假活动），历史数据需要按新规则重算一遍。原始事件不动，只重建 sessions 表，
// 因此这个操作可以反复执行。
//
// 参数：date=YYYY-MM-DD 重算单日；from/to=YYYY-MM-DD 重算区间（都会重算两端）。
// 需要管理令牌。
func (s *Server) handleRebuildSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 POST")
		return
	}
	if s.cfg.AdminToken == "" {
		writeError(w, http.StatusForbidden, "admin_disabled", "管理接口未开启")
		return
	}
	if subtleCompare(bearerToken(r), s.cfg.AdminToken) != 1 {
		_ = s.store.RecordSecurityEvent(r.Context(), "admin_rejected", "管理令牌无效", "")
		writeError(w, http.StatusUnauthorized, "unauthorized", "管理令牌无效")
		return
	}

	query := r.URL.Query()
	fromRaw, toRaw := query.Get("from"), query.Get("to")
	if date := query.Get("date"); date != "" {
		fromRaw, toRaw = date, date
	}
	if fromRaw == "" || toRaw == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"需要 date=YYYY-MM-DD 或 from/to=YYYY-MM-DD")
		return
	}

	loc := s.cfg.Location()
	from, err := time.ParseInLocation("2006-01-02", fromRaw, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", "from 格式必须是 YYYY-MM-DD")
		return
	}
	to, err := time.ParseInLocation("2006-01-02", toRaw, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", "to 格式必须是 YYYY-MM-DD")
		return
	}
	if to.Before(from) {
		writeError(w, http.StatusBadRequest, "invalid_range", "to 不能早于 from")
		return
	}
	// 限制单次重算跨度，避免误传参数导致长时间占用数据库。
	if days := int(to.Sub(from).Hours()/24) + 1; days > 92 {
		writeError(w, http.StatusBadRequest, "range_too_large", "单次最多重算 92 天")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	dates, err := s.engine.RebuildRange(ctx, from, to)
	if err != nil {
		s.logger.Error("重算 Session 失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "重算失败")
		return
	}

	summary := make([]map[string]any, 0, len(dates))
	for _, date := range dates {
		list, err := s.store.SessionsByDate(ctx, date)
		if err != nil {
			s.logger.Warn("查询重算结果失败", "date", date, "error", err.Error())
			continue
		}
		summary = append(summary, map[string]any{"date": date, "session_count": len(list)})
	}
	s.logger.Info("Session 重算完成", "from", fromRaw, "to", toRaw, "days", len(dates))
	writeJSON(w, http.StatusOK, map[string]any{
		"algorithm": s.engineAlgorithm(),
		"days":      summary,
	})
}

// engineAlgorithm 返回引擎使用的算法版本，便于运维确认重算规则。
func (s *Server) engineAlgorithm() string {
	if s.engine == nil {
		return ""
	}
	return sessions.AlgorithmVersion
}

// handleSummary 返回某天的已生成总结。
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 GET")
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().In(s.cfg.Location()).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, s.cfg.Location()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", "date 格式必须是 YYYY-MM-DD")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	record, ok, err := s.store.SummaryByDate(ctx, date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "查询失败")
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"date": date, "found": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"date":               date,
		"found":              true,
		"status":             record.Status,
		"summary_id":         record.ID,
		"model":              record.Model,
		"prompt_version":     record.PromptVersion,
		"rendered_text":      record.RenderedText,
		"structured":         json.RawMessage(nonEmptyJSON(record.StructuredOutput)),
		"source_session_ids": json.RawMessage(nonEmptyJSON(record.SourceSessionIDs)),
		"token_usage":        json.RawMessage(nonEmptyJSON(record.TokenUsage)),
		"error_code":         record.ErrorCode,
		"updated_at":         storage.FormatISO(record.UpdatedAt),
	})
}

func (s *Server) handleGenerateSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 POST")
		return
	}
	// 管理鉴权：只有持有 admin token 的调用者才能花钱触发模型。
	if s.cfg.AdminToken == "" {
		writeError(w, http.StatusForbidden, "admin_disabled", "管理接口未开启")
		return
	}
	token := bearerToken(r)
	if subtleCompare(token, s.cfg.AdminToken) != 1 {
		_ = s.store.RecordSecurityEvent(r.Context(), "admin_rejected", "管理令牌无效", "")
		writeError(w, http.StatusUnauthorized, "unauthorized", "管理令牌无效")
		return
	}

	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().In(s.cfg.Location()).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, s.cfg.Location()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", "date 格式必须是 YYYY-MM-DD")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	result, err := s.summary.GenerateForDate(ctx, date)
	if err != nil {
		s.logger.Error("手工生成总结失败", "date", date, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "生成失败")
		return
	}
	status := http.StatusOK
	if result.Status == "skipped" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{
		"date":       result.Date,
		"status":     result.Status,
		"summary_id": result.SummaryID,
		"skipped":    result.Skipped,
		"text":       result.Text,
	})
}

// handleAsk 用与飞书问答完全相同的逻辑处理一句话。
//
// 存在的意义：飞书是唯一交互入口，出问题时很难定位是"意图识别"、"检索"还是"模型"
// 出的错。这个接口把同一套逻辑暴露出来，便于直接验证与排查。
// 只读：不写 conversations 表，不发送任何消息。
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "只支持 POST")
		return
	}
	if s.cfg.AdminToken == "" {
		writeError(w, http.StatusForbidden, "admin_disabled", "管理接口未开启")
		return
	}
	if subtleCompare(bearerToken(r), s.cfg.AdminToken) != 1 {
		_ = s.store.RecordSecurityEvent(r.Context(), "admin_rejected", "管理令牌无效", "")
		writeError(w, http.StatusUnauthorized, "unauthorized", "管理令牌无效")
		return
	}
	if s.qa == nil {
		writeError(w, http.StatusServiceUnavailable, "qa_disabled", "问答服务未启用")
		return
	}

	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	// 走与飞书完全相同的 Agent 路径：模型出计划 → Policy Gate → 执行能力 → 合成。
	// 这里不再单独解析意图，否则"调试时看到的行为"会与飞书上的真实行为不一致。
	reply, err := s.qa.Handle(ctx, "debug", req.Text)
	if err != nil {
		s.logger.Error("问答处理失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "处理失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"question":           req.Text,
		"mode":               reply.Mode,
		"status":             reply.Status,
		"support_level":      reply.SupportLevel,
		"used_ai":            reply.UsedAI,
		"answer":             reply.Text,
		"tool_calls":         reply.ToolCalls,
		"denied_tools":       reply.DeniedTools,
		"source_session_ids": reply.SourceSessionIDs,
		// 任务摘要的来源单独一组：两类来源的可信度不同，合在一起就分不清
		// 回答里哪些是 Agent 报告的结论、哪些是从活动记录推断的。
		// 只用于追溯（管理令牌 + 只读），不进用户可见文本。
		"source_task_ids": reply.SourceTaskIDs,
	})
}

// ---- 辅助 ----

// authenticate 校验 Bearer device token。
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (storage.Device, bool) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "缺少凭证")
		return storage.Device{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	device, err := s.auth.Authenticate(ctx, token)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			// 只记录来源，不记录令牌内容。
			_ = s.store.RecordSecurityEvent(ctx, "auth_rejected", "设备凭证无效", clientIP(r))
			writeError(w, http.StatusUnauthorized, "unauthorized", "凭证无效")
			return storage.Device{}, false
		}
		s.logger.Error("鉴权失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "鉴权失败")
		return storage.Device{}, false
	}
	return device, true
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		if idx := strings.IndexByte(ip, ','); idx > 0 {
			return strings.TrimSpace(ip[:idx])
		}
		return strings.TrimSpace(ip)
	}
	return r.RemoteAddr
}

func decodeJSON(r *http.Request, dst any, maxBytes int64) error {
	body := io.LimitReader(r.Body, maxBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.New("请求体解析失败")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// renderSessions 把数据库记录转换成对外的 JSON 结构。
func renderSessions(list []storage.Session, loc *time.Location) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		out = append(out, map[string]any{
			"id":           s.ID,
			"date":         s.Date,
			"project":      s.Project,
			"start_at":     storage.FormatISO(s.StartAt),
			"end_at":       storage.FormatISO(s.EndAt),
			"start_local":  s.StartAt.In(loc).Format(time.RFC3339),
			"end_local":    s.EndAt.In(loc).Format(time.RFC3339),
			"apps":         json.RawMessage(nonEmptyJSONArr(s.AppsJSON)),
			"git":          json.RawMessage(nonEmptyJSONArr(s.GitJSON)),
			"stats":        json.RawMessage(nonEmptyJSONObject(s.StatsJSON)),
			"algorithm":    s.AlgorithmVersion,
			"source_range": []string{storage.FormatISO(s.SourceStartAt), storage.FormatISO(s.SourceEndAt)},
		})
	}
	return out
}

// renderTaskSummary 把任务摘要渲染成 API 响应。
//
// 时间同时给 UTC 与本地两种：调用方（含模型上下文组装）不该自己猜时区。
func renderTaskSummary(t storage.TaskSummary, loc *time.Location) map[string]any {
	out := map[string]any{
		"task_id":        t.TaskID,
		"title":          t.Title,
		"status":         t.Status,
		"outcomes":       nonNilList(t.Outcomes),
		"open_loops":     nonNilList(t.OpenLoops),
		"source_agent":   t.SourceAgent,
		"occurred_at":    storage.FormatISO(t.OccurredAt),
		"occurred_local": t.OccurredAt.In(loc).Format(time.RFC3339),
	}
	if t.Project != "" {
		out["project"] = t.Project
	}
	if t.App != "" {
		out["app"] = t.App
	}
	if t.SourceSessionID != "" {
		out["source_session_id"] = t.SourceSessionID
	}
	return out
}

// nonNilList 保证 JSON 里是 [] 而不是 null，避免调用方把 null 当成"没有数据"。
func nonNilList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonEmptyJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

func nonEmptyJSONArr(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "[]"
	}
	return raw
}

func nonEmptyJSONObject(raw string) string {
	if strings.TrimSpace(raw) == "" || raw == "{}" {
		return "{}"
	}
	return raw
}

// subtleCompare 使用标准库的 constant-time 比较，避免时序侧信道。
func subtleCompare(a, b string) int {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}
