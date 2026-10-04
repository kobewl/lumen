"""Additive upgrades for the v0.01 SQLite database."""


def migrate(db):
    columns = {
        'memories': {'status': "TEXT NOT NULL DEFAULT 'confirmed'", 'category': "TEXT NOT NULL DEFAULT 'personal'",
                     'expires_at': 'TEXT', 'source': "TEXT NOT NULL DEFAULT 'user'",
                     'catalog': "TEXT NOT NULL DEFAULT 'soul'", 'proposed_content': 'TEXT', 'proposed_key': 'TEXT',
                     'proposed_catalog': 'TEXT', 'proposed_expires_at': 'TEXT'},
        'feishu_inbox': {'sender':'TEXT','content':'TEXT'},
        'todos': {'priority': "TEXT NOT NULL DEFAULT 'normal'", 'project_id': 'TEXT',
                  'notes': "TEXT NOT NULL DEFAULT ''", 'completed_at': 'TEXT'},
        'schedules': {'todo_id': 'TEXT', 'month_day': 'INTEGER', 'created_by': "TEXT NOT NULL DEFAULT 'user'", 'wall_time':'TEXT', 'zone_name':'TEXT'},
    }
    with db:
        memory_columns = {row[1] for row in db.execute('PRAGMA table_info(memories)')}
        for table, additions in columns.items():
            existing = {row[1] for row in db.execute(f'PRAGMA table_info({table})')}
            for name, declaration in additions.items():
                if name not in existing:
                    db.execute(f'ALTER TABLE {table} ADD COLUMN {name} {declaration}')
        if 'catalog' not in memory_columns:
            db.execute("UPDATE memories SET catalog='daily' WHERE category IN ('work','temporary')")
        db.executescript('''
            CREATE TABLE IF NOT EXISTS projects (
                id TEXT PRIMARY KEY, title TEXT NOT NULL, goal TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'active', created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS notes (
                id TEXT PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL,
                tags TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS plans (
                id TEXT PRIMARY KEY, title TEXT NOT NULL, goal TEXT NOT NULL,
                steps TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
                project_id TEXT, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS changes (
                id TEXT PRIMARY KEY, action TEXT NOT NULL, record_id TEXT,
                changes TEXT NOT NULL, created_at TEXT NOT NULL,
                undone INTEGER NOT NULL DEFAULT 0, actor TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS chat_requests (
                id TEXT PRIMARY KEY, message TEXT NOT NULL, status TEXT NOT NULL,
                reply TEXT, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS usage (
                id TEXT PRIMARY KEY, model TEXT NOT NULL, input_tokens INTEGER NOT NULL,
                output_tokens INTEGER NOT NULL, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS memory_policies (
                id TEXT PRIMARY KEY, catalog TEXT NOT NULL, operation TEXT NOT NULL,
                mode TEXT NOT NULL, UNIQUE(catalog, operation));
            CREATE TABLE IF NOT EXISTS capture_events (
                id TEXT PRIMARY KEY, reason TEXT NOT NULL,
                attempted INTEGER NOT NULL, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS action_policies (
                id TEXT PRIMARY KEY, scope TEXT NOT NULL, operation TEXT NOT NULL,
                mode TEXT NOT NULL, UNIQUE(scope,operation));
            CREATE TABLE IF NOT EXISTS action_requests (
                id TEXT PRIMARY KEY, action TEXT NOT NULL, record_id TEXT NOT NULL,
                snapshot TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL,
                completed_at TEXT);
            CREATE INDEX IF NOT EXISTS action_requests_pending ON action_requests(status,action,record_id);
            CREATE INDEX IF NOT EXISTS schedules_due ON schedules(enabled,status,run_at);
            CREATE INDEX IF NOT EXISTS todos_due ON todos(done,due_at);
            CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries(status,next_at);
            PRAGMA user_version=10100;
        ''')
        from .policy import ensure
        ensure(db)
        from .permissions import ensure as ensure_actions
        ensure_actions(db)
        if 'purpose' not in {row[1] for row in db.execute('PRAGMA table_info(usage)')}:
            db.execute("ALTER TABLE usage ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'")
