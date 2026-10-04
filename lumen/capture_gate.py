"""Cheap capture screening and a separate, attempt-based classifier budget."""
import os
import re
from datetime import timezone
from .contracts import identifier,now,stamp
from .privacy import reasons

DEFAULTS={'LUMEN_CAPTURE_DAILY_CALL_LIMIT':24,'LUMEN_CAPTURE_CHAT_RESERVE':20,'LUMEN_CAPTURE_MAX_CHARS':1500}
_EXPLICIT=re.compile(r'记住|记一下|记下|更新.*(?:记忆|偏好)|更正|忘记|删除|\b(?:remember|forget|delete)\b',re.I)
_NONFACT=re.compile(r'帮我|提醒我|待办|定时|计划步骤|翻译|假设|如果|举例|假装|转述|他说|她说|小说|角色|\b(?:remind|translate|pretend|suppose)\b',re.I)
_SELF=re.compile(r'我(?:的|这|现在|最近|目前|还|一直|一向|平时|通常|一般|已经|是|叫|住|在|来自|喜欢|不喜欢|讨厌|偏爱|习惯|更喜欢|不吃|爱吃|不喝)|\b(?:i am|i live|i like|i prefer|i hate|my (?:name|job|favorite|schedule))\b',re.I)
_QUESTION=re.compile(r'我(?:喜欢什么|在哪里|叫什么)|还记得|是否|吗[？?]?$|\b(?:what is my|do you remember)\b',re.I)


def screen(text):
    if not isinstance(text,str) or not text.strip():return 'empty'
    if text.startswith('/'):return 'command'
    if reasons(text):return 'sensitive'
    if os.getenv('LUMEN_CAPTURE_ENABLED','true').lower()!='true':return 'disabled'
    if len(text)>int(os.getenv('LUMEN_CAPTURE_MAX_CHARS',DEFAULTS['LUMEN_CAPTURE_MAX_CHARS'])):return 'too_long'
    if _EXPLICIT.search(text):return 'explicit'
    if _NONFACT.search(text):return 'nonfact'
    if _QUESTION.search(text):return 'question'
    if not _SELF.search(text):return 'no_fact_signal'
    return 'candidate'


def claim(store,zone):
    """Reserve an attempt atomically; a failed API request still consumes this cap."""
    local=now().astimezone(zone)
    since=stamp(local.replace(hour=0,minute=0,second=0,microsecond=0).astimezone(timezone.utc))
    limit=int(os.getenv('LUMEN_CAPTURE_DAILY_CALL_LIMIT',DEFAULTS['LUMEN_CAPTURE_DAILY_CALL_LIMIT']))
    reserve=int(os.getenv('LUMEN_CAPTURE_CHAT_RESERVE',DEFAULTS['LUMEN_CAPTURE_CHAT_RESERVE']))
    global_limit=int(os.getenv('LUMEN_MODEL_DAILY_CALL_LIMIT','120'))
    with store.transaction() as db:
        attempts=db.execute('SELECT COUNT(*) FROM capture_events WHERE attempted=1 AND created_at>=?',(since,)).fetchone()[0]
        calls=db.execute('SELECT COUNT(*) FROM usage WHERE created_at>=?',(since,)).fetchone()[0]
        reason='capture_budget' if attempts>=limit else 'chat_reserve' if global_limit-calls<=reserve else 'started'
        event_id=identifier()
        db.execute('INSERT INTO capture_events VALUES (?,?,?,?)',(event_id,reason,int(reason=='started'),stamp()))
    return event_id,reason


def record(store,reason):
    with store.transaction() as db:
        db.execute('INSERT INTO capture_events VALUES (?,?,0,?)',(identifier(),reason,stamp()))
        # Keep diagnostics bounded; no text or extracted fact is recorded here.
        db.execute('DELETE FROM capture_events WHERE rowid NOT IN (SELECT rowid FROM capture_events ORDER BY rowid DESC LIMIT 10000) AND attempted=0')


def finish(store,event_id,reason):
    with store.transaction() as db:db.execute('UPDATE capture_events SET reason=? WHERE id=?',(reason,event_id))
