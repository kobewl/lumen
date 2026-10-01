"""Grounded daily/weekly reviews that work without an LLM."""
from datetime import datetime, timedelta, timezone
from .contracts import now, stamp


def briefing(actions, mode='today', at=None):
    at = at or now()
    local = at.astimezone(actions.zone)
    start = local.replace(hour=0, minute=0, second=0, microsecond=0)
    end = start + timedelta(days=1)
    todos = actions.store.query("SELECT * FROM todos WHERE done=0 ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,due_at IS NULL,due_at,created_at")
    overdue = [t for t in todos if t['due_at'] and datetime.fromisoformat(t['due_at']) < at]
    due = [t for t in todos if t['due_at'] and at <= datetime.fromisoformat(t['due_at']) < end]
    lines = [local.strftime('%Y-%m-%d')+' · '+('每周回顾' if mode=='weekly' else '今日简报'),
             f'未完成 {len(todos)} 项 · 已逾期 {len(overdue)} 项 · 今天到期 {len(due)} 项']
    if mode=='weekly':
        completed = actions.store.query('SELECT * FROM todos WHERE done=1 AND completed_at>=? ORDER BY completed_at DESC', (stamp((start-timedelta(days=6)).astimezone(timezone.utc)),))
        lines.append(f'最近七天完成 {len(completed)} 项')
        lines.extend('✓ '+t['title'][:160] for t in completed[:10])
    selected = (overdue + due + [t for t in todos if t not in overdue and t not in due])[:8]
    if selected:
        lines.append('接下来值得关注：')
        lines.extend('· '+t['title'][:160]+('（逾期）' if t in overdue else '（今天到期）' if t in due else '') for t in selected)
    else:
        lines.append('当前没有待处理事项，可以留一点时间给自己。')
    return {'date':local.date().isoformat(),'mode':mode,'open':len(todos),'overdue':len(overdue),
            'due_today':len(due),'content':'\n'.join(lines),'todo_ids':[t['id'] for t in selected]}
