"""Memory catalogs and who may write them without asking."""

CATALOGS = ('soul', 'daily')
OPERATIONS = ('capture', 'revise')
MODES = ('auto', 'confirm')
DEFAULTS = (
    ('soul', 'capture', 'auto'),
    ('soul', 'revise', 'confirm'),
    ('daily', 'capture', 'auto'),
    ('daily', 'revise', 'auto'),
)


def ensure(db):
    for catalog, operation, mode in DEFAULTS:
        db.execute(
            'INSERT OR IGNORE INTO memory_policies (id,catalog,operation,mode) VALUES (?,?,?,?)',
            (f'{catalog}:{operation}', catalog, operation, mode))


def grant(store, catalog, operation):
    rows = store.query(
        'SELECT mode FROM memory_policies WHERE catalog=? AND operation=?',
        (catalog, operation))
    if rows and rows[0]['mode'] in MODES:
        return rows[0]['mode']
    for item_catalog, item_operation, mode in DEFAULTS:
        if item_catalog == catalog and item_operation == operation:
            return mode
    return 'confirm'


def catalog_for(args, row):
    if args.get('catalog') in CATALOGS:
        return args['catalog']
    category = args.get('category', (row or {}).get('category'))
    if category in ('work', 'temporary'):
        return 'daily'
    existing = (row or {}).get('catalog')
    if existing in CATALOGS:
        return existing
    return 'soul'
