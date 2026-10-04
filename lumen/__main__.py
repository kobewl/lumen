from __future__ import annotations

import argparse
import hmac
import json
import logging
import os
import threading
import signal
import sqlite3
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

from . import __version__
from .agent import Agent, Model, ModelError
from .core import Actions, Scheduler, Store
from .diagnostics import status
from .operations import activity,backup,export_data,restore,ProcessLock
from .briefing import briefing

STATIC = Path(__file__).parent / 'static'


def create_server(host, port, store, actions, agent, token='', runtime=None):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            # Avoid logging request URLs or user data.
            pass

        def send(self, code, data, content_type='application/json; charset=utf-8'):
            raw = json.dumps(data, ensure_ascii=False).encode() if isinstance(data, (dict, list)) else data
            self.send_response(code)
            self.send_header('Content-Type', content_type)
            self.send_header('Content-Length', str(len(raw)))
            self.send_header('Cache-Control', 'no-store')
            self.send_header('X-Content-Type-Options', 'nosniff')
            self.send_header('Content-Security-Policy', "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
            self.end_headers()
            self.wfile.write(raw)

        def authorized(self):
            requested=urlparse('http://'+self.headers.get('Host','')).hostname
            allowed={'localhost','127.0.0.1','::1',*filter(None,os.getenv('LUMEN_ALLOWED_HOSTS','').split(','))}
            if host in ('127.0.0.1','localhost','::1') and requested not in allowed:
                self.send(403,{'error':'未允许的访问主机，请检查 LUMEN_ALLOWED_HOSTS'})
                return False
            origin = self.headers.get('Origin')
            if origin and urlparse(origin).netloc != self.headers.get('Host'):
                self.send(403, {'error': '拒绝跨站请求'})
                return False
            if token and not hmac.compare_digest(self.headers.get('Authorization', ''), 'Bearer ' + token):
                self.send(401, {'error': '请输入正确的访问令牌'})
                return False
            return True

        def do_GET(self):
            path = urlparse(self.path).path
            if path == '/api/health':
                return self.send(200, {'ok': True, 'version': __version__})
            if path.startswith('/api/'):
                if not self.authorized():
                    return
                if path == '/api/status':
                    return self.send(200,runtime() if runtime else status(store,actions,agent.model))
                if path == '/api/export':
                    return self.send(200,export_data(store))
                if path == '/api/backup':
                    path=backup(store,os.getenv('LUMEN_BACKUP_DIR'))
                    self.send_response(200)
                    self.send_header('Content-Type','application/vnd.sqlite3')
                    self.send_header('Content-Length',str(path.stat().st_size))
                    self.send_header('Content-Disposition','attachment; filename=lumen-backup.db')
                    self.send_header('Cache-Control','no-store')
                    self.end_headers()
                    with path.open('rb') as source:
                        while chunk:=source.read(65536):self.wfile.write(chunk)
                    return
                if path == '/api/state':
                    actions.context()
                    state = store.state()
                    state.update({'today':briefing(actions),'activity':activity(store),'diagnostics':runtime() if runtime else status(store,actions,agent.model)})
                    state.update({'timezone': actions.zone.key, 'model_configured': bool(getattr(agent.model, 'key', True)), 'feishu_enabled': agent.feishu_enabled})
                    return self.send(200, state)
                return self.send(404, {'error': '接口不存在'})
            files = {'/': ('index.html', 'text/html; charset=utf-8'),
                     '/app.js': ('app.js', 'text/javascript; charset=utf-8'),
                     '/style.css': ('style.css', 'text/css; charset=utf-8')}
            if path not in files:
                return self.send(404, {'error': '页面不存在'})
            filename, mime = files[path]
            self.send(200, (STATIC / filename).read_bytes(), mime)

        def do_POST(self):
            if not self.authorized():
                return
            try:
                size = int(self.headers.get('Content-Length', '0'))
                if size < 1 or size > 65536:
                    return self.send(413, {'error': '请求过大或为空'})
                self.connection.settimeout(10)
                data = json.loads(self.rfile.read(size))
                if not isinstance(data, dict):
                    raise ValueError('请求必须是 JSON 对象')
                path = urlparse(self.path).path
                if path == '/api/conversation/new':
                    return self.send(200, {'reply': agent.new_conversation()})
                if path == '/api/chat':
                    text = data.get('message')
                    if not isinstance(text, str) or not text.strip() or len(text) > 8000:
                        raise ValueError('请输入 1–8000 字符的消息')
                    request_id=data.get('request_id')
                    if request_id is not None and (not isinstance(request_id,str) or not 8<=len(request_id)<=100):
                        raise ValueError('request_id 必须是 8–100 字符字符串')
                    return self.send(200, {'reply': agent.reply(text.strip(),request_id=request_id)})
                if path == '/api/action':
                    with agent.lock:
                        result = actions.execute(data.get('name'), data.get('args'))
                    return self.send(200, result)
                return self.send(404, {'error': '接口不存在'})
            except ModelError as exc:
                self.send(502, {'error': str(exc)})
            except (ValueError, KeyError, TypeError) as exc:
                self.send(400, {'error': str(exc) if isinstance(exc, ValueError) else '参数无效，请检查字段、ID 和时间'})
            except Exception:
                logging.exception('request failed')
                self.send(500, {'error': '服务处理失败，请稍后重试'})

    return ThreadingHTTPServer((host, port), Handler)


def main():
    parser = argparse.ArgumentParser(description='Lumen '+__version__+' personal assistant')
    parser.add_argument('--host', default=os.getenv('LUMEN_HOST', '127.0.0.1'))
    parser.add_argument('--port', type=int, default=int(os.getenv('LUMEN_PORT', '8787')))
    parser.add_argument('--db', default=os.getenv('LUMEN_DB_PATH', 'data/lumen-v001.db'))
    parser.add_argument('--backup',metavar='DIRECTORY',help='备份数据库后退出（先停止服务）')
    parser.add_argument('--export',metavar='FILE',help='导出 JSON 后退出（先停止服务）')
    parser.add_argument('--restore',metavar='BACKUP',help='从 SQLite 备份恢复并退出（先停止服务）')
    parser.add_argument('--check',action='store_true',help='检查配置和数据库后退出，不连接外部平台')
    args = parser.parse_args()
    try:process_lock=ProcessLock(args.db)
    except ValueError as exc:parser.error(str(exc))
    if args.restore:
        try:restore(args.restore,args.db)
        except (ValueError,OSError,sqlite3.Error) as exc:parser.error(str(exc))
        finally:process_lock.close()
        print('数据库已恢复；请启动服务验证。')
        return
    for name,default,minimum,maximum in [('LUMEN_MODEL_DAILY_CALL_LIMIT','120',0,100000),('LUMEN_MODEL_TIMEOUT_SECONDS','45',1,120),('LUMEN_CAPTURE_DAILY_CALL_LIMIT','24',0,100000),('LUMEN_CAPTURE_CHAT_RESERVE','20',1,100000),('LUMEN_CAPTURE_MAX_CHARS','1500',1,8000)]:
        try:
            value=int(os.getenv(name,default))
            if not minimum<=value<=maximum:raise ValueError()
        except ValueError:parser.error(name+' 必须是允许范围内的整数')
    token = os.getenv('LUMEN_ACCESS_TOKEN', '')
    if args.host not in ('127.0.0.1', 'localhost', '::1') and len(token) < 24:
        parser.error('非本地监听必须设置至少 24 字符的 LUMEN_ACCESS_TOKEN')
    logging.basicConfig(level=logging.INFO)
    store = Store(args.db)
    actions = Actions(store, os.getenv('LUMEN_TIMEZONE', 'Asia/Shanghai'))
    agent = Agent(store, actions, Model())
    if args.backup or args.export or args.check:
        try:
            if args.backup:print('一致性备份：'+str(backup(store,args.backup)))
            if args.export:
                output=Path(args.export)
                if output.resolve()==Path(args.db).resolve():parser.error('导出文件不能覆盖当前数据库')
                output.parent.mkdir(parents=True,exist_ok=True)
                output.write_text(json.dumps(export_data(store),ensure_ascii=False,indent=2));os.chmod(output,0o600)
                print('个人数据已导出。')
            if args.check:
                print(json.dumps({'version':__version__,'database':store.query('PRAGMA integrity_check')[0]['integrity_check'],'model_configured':bool(agent.model.key),'feishu_credentials_present':bool(os.getenv('LUMEN_FEISHU_APP_ID') and os.getenv('LUMEN_FEISHU_APP_SECRET'))},ensure_ascii=False))
        finally:store.db.close();process_lock.close()
        return
    app_id = os.getenv('LUMEN_FEISHU_APP_ID', '')
    app_secret = os.getenv('LUMEN_FEISHU_APP_SECRET', '')
    owners = [x.strip() for x in os.getenv('LUMEN_FEISHU_ALLOWED_USER_IDS', '').split(',') if x.strip()]
    feishu = None
    if app_id or app_secret or owners:
        if not app_id or not app_secret or len(owners) != 1:
            parser.error('飞书需要 APP_ID、APP_SECRET 和恰好一个允许用户 open_id（单用户模式）')
        from .feishu import Feishu
        feishu = Feishu(store, agent, owners[0])
        try:
            feishu.start(app_id, app_secret)
        except RuntimeError as exc:
            parser.error(str(exc))
        agent.feishu_enabled = True
    scheduler = Scheduler(store, actions, agent, owners[0] if feishu else None)
    runtime=lambda:status(store,actions,agent.model,feishu,scheduler)
    agent.status_provider=runtime
    server = create_server(args.host, args.port, store, actions, agent, token,runtime)
    maintenance=None
    if os.getenv('LUMEN_AUTO_BACKUP','true').lower()=='true':
        from .maintenance import backup_loop
        maintenance=threading.Thread(target=backup_loop,args=(store,actions.zone,scheduler.stop),daemon=True)
        maintenance.start()
    worker = threading.Thread(target=scheduler.run, daemon=True)
    worker.start()
    logging.info('Lumen v%s listening on http://%s:%s', __version__, args.host, args.port)
    def interrupt(signum,frame):raise KeyboardInterrupt
    signal.signal(signal.SIGTERM,interrupt)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        scheduler.stop.set()
        if feishu:
            feishu.stop.set()
        server.server_close()
        worker.join(timeout=170)
        if maintenance:maintenance.join(timeout=10)
        process_lock.close()


if __name__ == '__main__':
    main()
