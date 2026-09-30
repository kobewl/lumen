"""Lumen macOS 采集端。

模块结构（对应设计文档的 sensors → event → privacy → storage → sync）：

    sensors/   窗口、空闲、Git 三个传感器，只产生事件，不联网
    event.py   事件模型与字段 allowlist
    privacy.py 路径/凭证过滤与项目白名单提取
    storage.py 本地 SQLite 与同步队列
    sync.py    批量上传与退避重试
    runtime.py 组装与生命周期
    cli.py     命令行入口
"""

__version__ = "0.1.0"
