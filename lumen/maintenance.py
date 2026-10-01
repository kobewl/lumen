import os
from pathlib import Path
from .contracts import now,stamp
from .operations import backup


def backup_loop(store,zone,stop):
    directory=os.getenv('LUMEN_BACKUP_DIR') or str(Path(store.path).parent/'backups')
    while not stop.is_set():
        try:
            date=now().astimezone(zone).date().isoformat()
            previous=store.query("SELECT value FROM settings WHERE key='backup_date'")
            if not previous or previous[0]['value']!=date:
                path=backup(store,directory)
                automatic=path.with_name('lumen-auto-'+path.name.removeprefix('lumen-'));path.rename(automatic)
                with store.transaction() as db:
                    for key,value in [('backup_date',date),('last_backup',stamp())]:
                        db.execute('INSERT INTO settings VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value',(key,value))
                files=sorted(Path(directory).glob('lumen-auto-*.db'),reverse=True)
                for old in files[7:]:old.unlink()
        except Exception:
            import logging
            logging.exception('automatic backup failed')
        stop.wait(3600)
