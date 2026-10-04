"""Local, deterministic secret checks. Never include detected values in errors."""
import json
import re
import unicodedata

MARKER = '[敏感信息已拦截]'
_ZERO_WIDTH = re.compile(r'[\u200b-\u200f\u202a-\u202e\u2060-\u206f\ufeff]')
_CREDENTIAL_LABEL = r'(?:密码|口令|验证码|密钥|私钥|访问令牌|安全码|password|passwd|pwd|pin|otp|api[_ -]?key|access[_ -]?token|client[_ -]?secret)'
_ID_LABEL = r'(?:身份证(?:号(?:码)?)?|证件号(?:码)?|护照(?:号(?:码)?)?|passport(?: number)?|identity number)'
_CARD_LABEL = r'(?:银行卡(?:号)?|信用卡(?:号)?|储蓄卡(?:号)?|卡号|card number|cvv|cvc)'
_LABELS = [('凭据', _CREDENTIAL_LABEL), ('证件', _ID_LABEL), ('银行卡', _CARD_LABEL)]
_ASSIGNED = re.compile(_CREDENTIAL_LABEL + r'''["'”’]?\s*(?:是|为|改成|is\b|[:=：])\s*["'“‘]?[\w!@#$%^&*.+/=-]+''', re.I)
_DOCUMENT = re.compile(_ID_LABEL + r'''["'”’]?\s*(?:是|为|is\b|[:=：])?\s*["'“‘]?[a-z\d][a-z\d -]{4,}''', re.I)
_CARD = re.compile(_CARD_LABEL + r'''["'”’]?\s*(?:是|为|is\b|[:=：])?\s*["'“‘]?\d[\d -]{2,}''', re.I)
_NUMBER = re.compile(r'(?<![\da-fA-F])\d(?:[ -]?\d){11,21}[xX]?(?![\da-fA-F])')
_TOKEN = re.compile(r'\b(?:sk-[\w-]{16,}|gh[pousr]_[\w]{20,}|github_pat_[\w]{30,}|xox[baprs]-[\w-]{20,}|AKIA[A-Z0-9]{16})\b')
_JWT = re.compile(r'\beyJ[\w-]+\.[\w-]+\.[\w-]{12,}\b')
_PRIVATE = re.compile(r'-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----')
_URL_PASSWORD = re.compile(r'https?://[^\s/@:]+:[^\s/@]+@')


def normalize(text):
    return _ZERO_WIDTH.sub('', unicodedata.normalize('NFKC', text))


def _luhn(value):
    total = 0
    for index, digit in enumerate(reversed(value)):
        number = int(digit)
        if index % 2:
            number *= 2
            if number > 9:
                number -= 9
        total += number
    return total % 10 == 0 and len(set(value)) > 1


def reasons(text):
    """Report categories only; deliberately conservative for labeled identifiers."""
    if not isinstance(text, str):
        return ()
    text = normalize(text)
    if text.lstrip().startswith(('{','[','"')):
        try:
            decoded=json.loads(text)
            text=normalize(decoded) if isinstance(decoded,str) else json.dumps(decoded,ensure_ascii=False)
        except ValueError:
            pass
    found = set()
    if _ASSIGNED.search(text) or _TOKEN.search(text) or _JWT.search(text) or _PRIVATE.search(text) or _URL_PASSWORD.search(text):
        found.add('凭据')
    if _DOCUMENT.search(text):
        found.add('证件')
    if _CARD.search(text):
        found.add('银行卡')
    for match in _NUMBER.finditer(text):
        number = re.sub(r'[ -]', '', match.group())
        if re.fullmatch(r'\d{6}(?:18|19|20)\d{2}[01]\d[0-3]\d\d{3}[\dXx]', number) or re.fullmatch(r'\d{6}\d{2}[01]\d[0-3]\d\d{3}', number):
            found.add('证件')
        elif number.isdigit() and 13 <= len(number) <= 19 and _luhn(number):
            found.add('银行卡')
    return tuple(sorted(found))


def memory_reasons(key, content):
    found = set(reasons(key + '\n' + content))
    normalized = normalize(key)
    for kind, label in _LABELS:
        if re.search(label+r'\s*$', normalized, re.I):
            found.add(kind)
    return tuple(sorted(found))


def blocked_memory(row):
    return bool(memory_reasons(row.get('key') or '', row.get('content') or '')
                or (row.get('proposed_content') and memory_reasons(row.get('proposed_key') or row.get('key') or '', row['proposed_content'])))


def require_safe_memory(key, content):
    found = memory_reasons(key, content)
    if found:
        raise ValueError('敏感信息不能保存为记忆（' + '、'.join(found) + '）；请使用密码或证件管理工具')


def redact(text):
    # Removing the entire affected line also covers unlabeled secrets on that line.
    if not isinstance(text, str) or not reasons(text):
        return text
    if _PRIVATE.search(normalize(text)):
        return MARKER
    lines = text.splitlines(keepends=True)
    redacted = ''.join(MARKER + ('\n' if line.endswith('\n') else '') if reasons(line) else line for line in lines)
    # A value split over lines can be detected only after normalization of whitespace.
    return MARKER if reasons(redacted) else redacted


def sanitize(value):
    """Apply to every outbound message, including tool results and old history."""
    if isinstance(value, str):
        if value.lstrip().startswith(('{','[')):
            try:
                structured=json.loads(value)
                if isinstance(structured,(dict,list)):
                    return json.dumps(sanitize(structured),ensure_ascii=False)
            except (ValueError,RecursionError):
                pass
        return redact(value)
    if isinstance(value, list):
        return [sanitize(item) for item in value]
    if isinstance(value, dict):
        result = {}
        label=value.get('key') or value.get('title')
        protected=isinstance(label,str) and isinstance(value.get('content'),str) and bool(memory_reasons(label,value['content']))
        for key, item in value.items():
            if isinstance(key,str) and isinstance(item,(str,int,float)) and memory_reasons(key,str(item)):
                result[key]=MARKER
                continue
            if protected and key in ('content','proposed_content'):
                result[key]=MARKER
                continue
            if key == 'arguments' and isinstance(item, str):
                try:
                    item = json.dumps(sanitize(json.loads(item)), ensure_ascii=False)
                except (ValueError, TypeError):
                    item = redact(item)
            else:
                item = sanitize(item)
            result[key] = item
        return result
    return value
