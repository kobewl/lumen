import os
from datetime import datetime,timedelta,timezone
from . import __version__
from .contracts import now, stamp


def status(store,actions,model,feishu=None,scheduler=None):
    local=now().astimezone(actions.zone)
    since=stamp(local.replace(hour=0,minute=0,second=0,microsecond=0).astimezone(timezone.utc))
    usage=store.query('SELECT COUNT(*) AS calls,COALESCE(SUM(input_tokens),0) AS input_tokens,COALESCE(SUM(output_tokens),0) AS output_tokens FROM usage WHERE created_at>=?',(since,))[0]
    capture=store.query('SELECT reason,attempted,COUNT(*) AS n FROM capture_events WHERE created_at>=? GROUP BY reason,attempted',(since,))
    return {'version':__version__,'timezone':actions.zone.key,'model':getattr(model,'name','test-model'),
            'model_configured':bool(getattr(model,'key',True)),'usage_today':usage,
            'model_call_limit':int(os.getenv('LUMEN_MODEL_DAILY_CALL_LIMIT','120')),
            'capture':{'attempts':sum(row['n'] for row in capture if row['attempted']),
                       'limit':int(os.getenv('LUMEN_CAPTURE_DAILY_CALL_LIMIT','24')),
                       'events':capture},
            'usage_by_purpose':store.query('SELECT purpose,COUNT(*) AS calls FROM usage WHERE created_at>=? GROUP BY purpose',(since,)),
            'feishu':feishu.status() if feishu else {'enabled':False,'long_connection':'disabled'},
            'pending_deliveries':store.query("SELECT COUNT(*) AS n FROM deliveries WHERE status='pending'")[0]['n'],
            'failed_tasks':store.query("SELECT COUNT(*) AS n FROM schedules WHERE status='failed'")[0]['n'],
            'interrupted_messages':store.query("SELECT COUNT(*) AS n FROM feishu_inbox WHERE status='interrupted'")[0]['n'],
            'scheduler_last_tick':scheduler.last_tick if scheduler else None,
            'last_backup':store.query("SELECT value FROM settings WHERE key='last_backup'")}
