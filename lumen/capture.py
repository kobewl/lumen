"""Small classifier that files facts into soul or daily before the chat model answers."""
import json
import logging
from .privacy import reasons, blocked_memory

PROMPT = '''你是记忆捕获器，不是聊天助手。只根据用户这一句话抽取值得记住的事实，输出 JSON：{"items":[{"catalog":"soul","key":"短名称","content":"一个事实","expires_at":""}]}
目录：
- soul：称呼、价值观、性格、稳定偏好、长期喜恶。
- daily：行程、所在地、近况、这阵子在做的事、会过期的状态。
规则：
- 没有这类事实就返回 {"items":[]}。
- 不记录待办、提醒、计划步骤、对助手的一次性要求、寒暄和猜测。
- 绝不记录密码、验证码、密钥、证件号码或银行卡号，即使用户要求记住。
- 同一事实沿用已有 key，不要新建近义名称。
- expires_at 只在用户说出明确期限时填写带时区的 ISO8601，否则用空字符串。
- 最多 4 条。key 不超过 40 字，content 不超过 400 字。'''


def parse_items(raw):
    if not isinstance(raw, str) or not raw.strip():
        return []
    start, end = raw.find('{'), raw.rfind('}')
    if start < 0 or end <= start:
        return []
    try:
        data = json.loads(raw[start:end + 1])
    except json.JSONDecodeError:
        return []
    items = data.get('items') if isinstance(data, dict) else None
    if not isinstance(items, list):
        return []
    parsed = []
    for item in items[:4]:
        if not isinstance(item, dict):
            continue
        catalog, key, content = item.get('catalog'), item.get('key'), item.get('content')
        if catalog not in ('soul', 'daily') or not isinstance(key, str) or not isinstance(content, str):
            continue
        key, content = key.strip()[:40], content.strip()[:400]
        if not key or not content:
            continue
        args = {'catalog': catalog, 'key': key, 'content': content}
        expires = item.get('expires_at')
        if isinstance(expires, str) and expires.strip():
            args['expires_at'] = expires.strip()
        parsed.append(args)
    return parsed


def file_utterance(model, actions, text):
    classify = getattr(model, 'classify', None)
    if not callable(classify) or not text or text.startswith('/') or reasons(text):
        return []
    try:
        from .contracts import stamp
        memories = actions.store.query(
            "SELECT catalog,key,content FROM memories WHERE status='confirmed' AND (expires_at IS NULL OR expires_at>?) ORDER BY updated_at DESC LIMIT 30",
            (stamp(),))
        raw = classify(text, [item for item in memories if not blocked_memory(item)])
        items = parse_items(raw)
    except Exception:
        logging.warning('memory capture skipped after classifier failure')
        return []
    filed = []
    for item in items:
        try:
            result = actions.execute('save_memory', item, actor='capture')
        except ValueError as exc:
            filed.append({'ok': False, 'key': item['key'], 'catalog': item['catalog'], 'error': str(exc)})
            continue
        filed.append({
            'ok': True, 'id': result['id'], 'key': item['key'], 'catalog': result.get('catalog', item['catalog']),
            'effect': result.get('effect', 'stored'), 'status': result.get('status'),
        })
    return filed
