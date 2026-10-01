import logging
import threading
from datetime import datetime
from zoneinfo import ZoneInfo
from .contracts import identifier, now, stamp
from .calendar import next_occurrence
from .briefing import briefing


class Scheduler:
    def __init__(self, store, actions, agent, notify_owner=None):
        self.store, self.actions, self.agent = store, actions, agent
        self.notify_owner = notify_owner
        self.stop = threading.Event()
        self.last_tick = None

    def tick(self, at=None):
        at = at or now()
        self.last_tick = stamp(at)
        due = self.store.query("SELECT * FROM schedules WHERE enabled=1 AND status='pending' AND run_at<=? ORDER BY run_at", (stamp(at),))
        for item in due:
            if self.stop.is_set():
                break
            with self.store.transaction() as db:
                claimed = db.execute("UPDATE schedules SET status='running' WHERE id=? AND status='pending' AND enabled=1", (item['id'],)).rowcount
            if not claimed:
                continue
            status, result = 'success', item['prompt']
            try:
                if item['kind']=='agent':
                    result = self.agent.reply('执行定时任务：'+item['prompt'],scheduled=True)
                elif item['kind']=='briefing':
                    result = briefing(self.actions,'weekly' if item['prompt']=='weekly' else 'today',at)['content']
            except Exception as exc:
                status, result = 'failed', f'任务「{item["title"]}」执行失败：{exc}'
            next_at = item['run_at']
            enabled = 0
            if item['repeat']!='none':
                zone = ZoneInfo(item['zone_name']) if item.get('zone_name') else self.actions.zone
                next_at = next_occurrence(next_at,item['repeat'],zone,max(at,now()),item.get('month_day'),item.get('wall_time'))
                enabled = int(status=='success')
            text = f'⏰ {item["title"]}\n{result}'
            with self.store.transaction() as db:
                db.execute('INSERT INTO runs VALUES (?,?,?,?,?,?)',(identifier(),item['id'],item['run_at'],status,result,stamp()))
                db.execute('INSERT INTO messages VALUES (?,?,?,?,?)',(identifier(),'assistant',text,stamp(),'schedule'))
                if self.notify_owner:
                    from .feishu import enqueue
                    enqueue(db,self.notify_owner,text,'schedule:'+item['id']+':'+item['run_at'])
                db.execute('UPDATE schedules SET status=?,enabled=?,run_at=?,last_error=? WHERE id=?',
                           ('pending' if enabled else status,enabled,next_at,result if status=='failed' else None,item['id']))

    def run(self):
        while not self.stop.is_set():
            try:self.tick()
            except Exception:logging.exception('scheduler tick failed')
            self.stop.wait(1)
