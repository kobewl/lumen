"""Feishu personal-chat adapter and durable outbound delivery."""
from __future__ import annotations

import asyncio
import json
import logging
import queue
import threading

from .core import identifier, now, stamp
from datetime import timedelta


class Feishu:
    def __init__(self, store, agent, owner, sender=None):
        self.store, self.agent, self.owner = store, agent, owner
        self.sender = sender
        self.stop = threading.Event()
        self.incoming = queue.Queue(maxsize=100)

    def handle(self, message_id, sender_id, chat_type, message_type, content):
        # Personal data belongs to one owner; never process group conversations.
        if sender_id != self.owner or chat_type != 'p2p' or not message_id:
            return
        if message_type != 'text':
            return
        try:
            text = json.loads(content).get('text', '').strip()
        except (ValueError, AttributeError):
            return
        if not text or len(text) > 8000:
            return
        with self.store.transaction() as db:
            claimed = db.execute('INSERT OR IGNORE INTO feishu_inbox VALUES (?,?,?)',
                                 (message_id, 'processing', stamp())).rowcount
        if not claimed:
            return
        try:
            reply = self.agent.reply(text)
        except Exception as exc:
            from .agent import ModelError
            reply = str(exc) if isinstance(exc, ModelError) else '处理失败，请查看管理面板后再试。'
        with self.store.transaction() as db:
            enqueue(db, self.owner, reply, 'reply:' + message_id)
            db.execute("UPDATE feishu_inbox SET status='done' WHERE id=?", (message_id,))

    def event(self, data):
        try:
            event = data.event
            message = event.message
            values = (message.message_id, event.sender.sender_id.open_id,
                      message.chat_type, message.message_type, message.content)
            self.incoming.put_nowait(values)
        except queue.Full:
            logging.error('Feishu incoming queue is full')
        except (AttributeError, TypeError):
            logging.warning('Invalid Feishu message event')

    def consume(self):
        while not self.stop.is_set():
            try:
                values = self.incoming.get(timeout=1)
            except queue.Empty:
                continue
            try:
                self.handle(*values)
            except Exception:
                logging.exception('Feishu message handling failed')
            finally:
                self.incoming.task_done()

    def deliver(self):
        pending = self.store.query("SELECT * FROM deliveries WHERE status='pending' AND next_at<=? ORDER BY rowid LIMIT 10", (stamp(),))
        for row in pending:
            try:
                self.sender(row['recipient'], row['content'], row['id'])
            except Exception:
                delay = min(3600, 2 ** min(row['attempts'] + 1, 12))
                with self.store.transaction() as db:
                    db.execute('UPDATE deliveries SET attempts=attempts+1,next_at=?,last_error=? WHERE id=?',
                               (stamp(now()+timedelta(seconds=delay)), '飞书发送失败，等待重试', row['id']))
            else:
                with self.store.transaction() as db:
                    db.execute("UPDATE deliveries SET status='sent',attempts=attempts+1,last_error=NULL WHERE id=?", (row['id'],))

    def delivery_loop(self):
        while not self.stop.is_set():
            try:
                self.deliver()
            except Exception:
                logging.exception('Feishu delivery failed')
            self.stop.wait(1)

    def start(self, app_id, app_secret):
        # The official SDK binds an asyncio loop during import.
        ready = threading.Event()
        failures = []
        def connect():
            asyncio.set_event_loop(asyncio.new_event_loop())
            try:
                import lark_oapi as lark
                from lark_oapi.api.im.v1 import CreateMessageRequest, CreateMessageRequestBody
                client = lark.Client.builder().app_id(app_id).app_secret(app_secret).timeout(30).log_level(lark.LogLevel.ERROR).build()
                def send(recipient, text, delivery_id):
                    request = CreateMessageRequest.builder().receive_id_type('open_id').request_body(
                        CreateMessageRequestBody.builder().receive_id(recipient).msg_type('text')
                        .content(json.dumps({'text': text}, ensure_ascii=False)).uuid(delivery_id).build()).build()
                    response = client.im.v1.message.create(request)
                    if not response.success():
                        raise RuntimeError('Feishu rejected message')
                self.sender = send
                handler = lark.EventDispatcherHandler.builder('', '').register_p2_im_message_receive_v1(self.event).build()
                ws = lark.ws.Client(app_id, app_secret, event_handler=handler, log_level=lark.LogLevel.ERROR)
            except Exception as exc:
                failures.append(exc)
                ready.set()
                return
            ready.set()
            while not self.stop.is_set():
                try:
                    ws.start()
                except Exception:
                    logging.error('Feishu long connection failed; retrying in 30 seconds')
                self.stop.wait(30)
        threading.Thread(target=connect, daemon=True).start()
        ready.wait(15)
        if failures or not self.sender:
            raise RuntimeError('飞书 SDK 初始化失败，请安装 requirements.txt 并检查配置')
        threading.Thread(target=self.consume, daemon=True).start()
        threading.Thread(target=self.delivery_loop, daemon=True).start()


def enqueue(db, recipient, text, event_key):
    # Split long answers to fit Feishu text message size. Stable UUIDs dedupe retries.
    for index, start in enumerate(range(0, len(text), 2500)):
        db.execute('INSERT OR IGNORE INTO deliveries (id,event_key,recipient,content,status,attempts,next_at) VALUES (?,?,?,?,?,?,?)',
                   (identifier(), f'{event_key}:{index}', recipient, text[start:start+2500], 'pending', 0, stamp()))
