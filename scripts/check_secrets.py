"""Check the Git index before publishing; never print matched secret values."""
import ipaddress
import re
import subprocess
import sys
from pathlib import PurePosixPath

PATTERNS = {
    'private key': r'-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----',
    'API token': r'\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{30,}|xox[baprs]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16})\b',
    'Feishu user identifier': r'\bou_[A-Za-z0-9]{24,}\b',
    'credential in URL': r'https?://[^\s/@:]+:[^\s/@]+@',
}


def git(*args):
    return subprocess.check_output(['git', *args])


def main():
    entries = git('ls-files', '--stage', '-z').split(b'\0')
    findings = []
    count = 0
    for entry in entries:
        if not entry:
            continue
        metadata, raw_path = entry.split(b'\t', 1)
        mode, sha, stage = metadata.decode().split()
        path = raw_path.decode()
        count += 1
        name = PurePosixPath(path).name
        parts = PurePosixPath(path).parts
        if stage != '0' or mode not in ('100644', '100755'):
            findings.append((path, 0, 'unexpected index entry'))
            continue
        sensitive_file = (name in ('.env', 'lumen.env', 'config.local.json')
                          or (name.startswith('.env.') and not name.endswith('.example'))
                          or name.endswith(('.pem', '.key', '.p12', '.pfx', '.db', '.sqlite', '.sqlite3', '.log', '.pyc', '.token'))
                          or any(p in ('secrets', 'credentials', '.ssh', 'data', 'backups', '__pycache__', '.venv') for p in parts))
        if sensitive_file:
            findings.append((path, 0, 'sensitive or runtime file'))
        raw = git('cat-file', 'blob', sha)
        if b'\0' in raw:
            findings.append((path, 0, 'binary file requires manual review'))
            continue
        text = raw.decode('utf-8')
        for number, line in enumerate(text.splitlines(), 1):
            for label, pattern in PATTERNS.items():
                if re.search(pattern, line):
                    findings.append((path, number, label))
            for address in re.findall(r'(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])', line):
                try:
                    ip = ipaddress.ip_address(address)
                except ValueError:
                    continue
                if ip.is_global:
                    findings.append((path, number, 'public IP address requires review'))
    if findings:
        for path, line, kind in findings:
            print(f'{path}:{line}: {kind}')
        print('Publish check failed. Values are intentionally redacted.')
        return 1
    print(f'Publish check passed: {count} indexed files; no secret patterns, public IP addresses, or runtime files found.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
