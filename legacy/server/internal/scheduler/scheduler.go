// Package scheduler 负责轻量定时任务。
//
// V0.1 只有三个任务，全部在单进程内用 ticker 实现，不引入 cron 服务：
//   - 每日总结：本地时间 22:30 触发一次；
//   - 事件保留清理：每天一次小批量删除过期事件；
//   - 数据快照：每天一次 SQLite 一致性备份 + WAL checkpoint。
package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"lumen/server/internal/initiative"
	"lumen/server/internal/storage"
	"lumen/server/internal/summary"
)

// Scheduler 持有定时任务所需的依赖。
type Scheduler struct {
	store         *storage.Store
	summary       *summary.Service
	loc           *time.Location
	summaryHour   int
	summaryMinute int
	retentionDays int
	backupDir     string
	backupKeep    int
	logger        *slog.Logger
	// initiative 是主动关怀服务（可选；nil = 未启用，不启动该任务）。
	initiative *initiative.Service
}

// New 创建调度器。
func New(store *storage.Store, summaryService *summary.Service, loc *time.Location,
	summaryHour, summaryMinute, retentionDays int, backupDir string, backupKeep int, logger *slog.Logger) *Scheduler {
	if loc == nil {
		loc = time.UTC
	}
	if backupKeep <= 0 {
		backupKeep = 7
	}
	return &Scheduler{
		store: store, summary: summaryService, loc: loc,
		summaryHour: summaryHour, summaryMinute: summaryMinute,
		retentionDays: retentionDays, backupDir: backupDir, backupKeep: backupKeep, logger: logger,
	}
}

// WithInitiative 注册主动关怀服务（链式，供装配根显式注入）。
//
// 频率与静默由 initiative.Policy 在服务内部守门；调度器只负责每 30 分钟
// 唤醒一次——被策略跳过时它不调模型、不写 outbox，代价可以忽略。
func (s *Scheduler) WithInitiative(svc *initiative.Service) *Scheduler {
	s.initiative = svc
	return s
}

// Run 阻塞运行所有定时任务，直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	go s.runSummaryJob(ctx)
	go s.runMaintenanceJob(ctx)
	if s.initiative != nil {
		go s.runInitiativeJob(ctx)
	}
}

// runInitiativeJob 每 30 分钟询问一次"要不要主动说一句话"。
//
// 判定（静默/频率/间隔）在服务内部完成，这里只负责唤醒与记录结果。
func (s *Scheduler) runInitiativeJob(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			out, err := s.initiative.RunOnce(runCtx, false)
			cancel()
			if err != nil {
				s.logger.Warn("主动关怀执行失败", "error", err.Error())
				continue
			}
			if out.Action != "disabled" && out.Action != "skipped" {
				s.logger.Info("主动关怀触发", "action", out.Action, "reason", out.Reason)
			}
		}
	}
}

// runSummaryJob 每分钟检查一次是否到达每日总结时间。
//
// 使用“日期 + 已完成”判断而不是长睡眠，这样服务重启后当天仍能补生成，
// 同时依赖 summary 服务的 input_hash 幂等保证不会重复调用模型。
func (s *Scheduler) runSummaryJob(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	// 启动时先检查一次，处理“服务在 22:30 之后才启动”的情况。
	s.trySummary(ctx, time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.trySummary(ctx, time.Now())
		}
	}
}

func (s *Scheduler) trySummary(ctx context.Context, now time.Time) {
	local := now.In(s.loc)
	target := time.Date(local.Year(), local.Month(), local.Day(), s.summaryHour, s.summaryMinute, 0, 0, s.loc)

	// 未到时间不触发。
	if local.Before(target) {
		return
	}

	date := local.Format("2006-01-02")

	// 注意：这里刻意**不**做"已有成功总结就跳过"的判断。
	//
	// 曾经有过那样的判断，它有一个严重缺陷：只要当天存在任意一份成功总结就跳过，
	// 完全不看数据是否已经变化。后果是——如果白天手工生成过一次总结（例如联调、
	// 补发），晚上的自动总结就永远不会用当天完整数据重新生成，用户收到的是残缺版本。
	//
	// 幂等交给 GenerateForDate 处理：它按 input_hash 判断，数据未变时直接复用
	// 已有结果（不调用模型、不消耗预算），数据变了才会真正重新生成。
	// 每日预算上限（默认 2 次）防止数据频繁变化时反复调用模型。
	s.logger.Info("触发每日总结", "date", date)
	// 使用带超时的子上下文，避免长时间占用调度循环。
	runCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()

	result, err := s.summary.GenerateForDate(runCtx, date)
	if err != nil {
		s.logger.Error("每日总结执行失败", "date", date, "error", err.Error())
		return
	}
	s.logger.Info("每日总结结束", "date", result.Date, "status", result.Status, "skipped", result.Skipped)
}

// runMaintenanceJob 每小时检查一次，只在凌晨执行保留清理与备份。
func (s *Scheduler) runMaintenanceJob(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			local := time.Now().In(s.loc)
			if local.Hour() != 3 {
				continue
			}
			s.maintain(ctx)
		}
	}
}

func (s *Scheduler) maintain(ctx context.Context) {
	// 1. 过期事件小批量删除。
	if s.retentionDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.retentionDays)
		total := int64(0)
		for {
			n, err := s.store.DeleteEventsBefore(ctx, cutoff, 5000)
			if err != nil {
				s.logger.Error("清理过期事件失败", "error", err.Error())
				break
			}
			total += n
			if n == 0 {
				break
			}
		}
		if total > 0 {
			s.logger.Info("清理过期事件完成", "deleted", total, "cutoff", cutoff.Format(time.RFC3339))
		}
	}

	// 2. WAL checkpoint，控制 WAL 文件大小。
	if err := s.store.Checkpoint(ctx); err != nil {
		s.logger.Warn("WAL checkpoint 失败", "error", err.Error())
	}

	// 3. 一致性快照。
	if s.backupDir != "" {
		name := "lumen-" + time.Now().In(s.loc).Format("20060102-150405") + ".db"
		dest := filepath.Join(s.backupDir, name)
		if err := s.store.Backup(ctx, dest); err != nil {
			s.logger.Error("生成数据库快照失败", "error", err.Error())
		} else {
			s.logger.Info("生成数据库快照完成", "path", dest)
			s.pruneBackups()
		}
	}
}

// pruneBackups 只保留最近 backupKeep 份快照，避免磁盘被历史备份占满。
func (s *Scheduler) pruneBackups() {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return
	}
	var backups []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".db" {
			backups = append(backups, e)
		}
	}
	if len(backups) <= s.backupKeep {
		return
	}
	// 文件名包含时间戳，字典序即时间序。
	for i := 0; i < len(backups)-s.backupKeep; i++ {
		path := filepath.Join(s.backupDir, backups[i].Name())
		if err := os.Remove(path); err != nil {
			s.logger.Warn("清理旧快照失败", "path", path, "error", err.Error())
		}
	}
}

// MaintainNow 立即执行一次维护流程，供运维手工触发。
func (s *Scheduler) MaintainNow(ctx context.Context) {
	s.maintain(ctx)
}
