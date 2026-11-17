"""结果记录器（M1 第 18 天，实施规划 §5）：实验网络内的 JSONL 追加写服务。

consumer 每处理一条消息 POST 业务结果到此；记录器追加写 per-execution JSONL
（共享卷），行格式 {message_id, event, ts}；message_id 重复出现在 PROCESSED
→ 额外追加 DUP_BUSINESS 事件（决策 5：message_id 为业务幂等键）。

纯 stdlib：本文件将随第 20 天三合一镜像进容器，零依赖不给镜像增包。
ts 语义（决策 11）：consumer 上报的事件时间戳优先，缺失才记服务器 UTC now。

用法（第 20 天起为容器入口）：python recorder.py [--port 8080] [--jsonl-path /data/records.jsonl]
第 18 天验收在宿主机直接跑。
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from lab.helpers.events import EVENT_DUP_BUSINESS as DUP_BUSINESS
from lab.helpers.events import EVENT_PROCESSED as PROCESSED

log = logging.getLogger("brp-recorder")


class Recorder:
    """JSONL 追加写 + PROCESSED 去重语义（线程安全：ThreadingHTTPServer 并发 POST）。"""

    def __init__(self, jsonl_path: Path | str):
        self.path = Path(jsonl_path)
        self._seen_processed: set[str] = set()
        self._lock = threading.Lock()
        self._replay_existing()

    def _replay_existing(self) -> None:
        """启动重放：卷内可能已有本执行早前写入（容器重建、卷保留形态）。

        坏行（上次写入中断的尾巴）跳过不崩；好行中的 PROCESSED 重建 seen 集。
        只认非空字符串 message_id（与 record() 入口校验同口径，跨重启一致）。
        """
        if not self.path.exists():
            return
        replayed = 0
        for line in self.path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            try:
                record = json.loads(line)
            except ValueError:
                continue
            replayed += 1
            message_id = record.get("message_id")
            if (
                record.get("event") == PROCESSED
                and isinstance(message_id, str)
                and message_id
            ):
                self._seen_processed.add(message_id)
        log.info(
            "replayed %d line(s) from %s, rebuilt seen set with %d processed id(s)",
            replayed, self.path, len(self._seen_processed),
        )

    @staticmethod
    def _validate(message_id: str, event: str, ts: str | None) -> None:
        """行格式契约的唯一执行点（评审 I-1）：类型/空值校验，坏字段进不了文件。"""
        if not isinstance(message_id, str) or not message_id:
            raise ValueError("message_id must be a non-empty string")
        if not isinstance(event, str) or not event:
            raise ValueError("event must be a non-empty string")
        if ts is not None and not isinstance(ts, str):
            raise ValueError("ts must be an ISO 8601 string or omitted")

    def record(self, message_id: str, event: str, ts: str | None = None) -> list[dict]:
        """记录一条业务事件，返回本次追加的行（重复 PROCESSED 额外附 DUP_BUSINESS 行）。

        校验→去重判定→文件追加在同一锁内：并发同 id POST 时行序与因果一致。
        """
        effective_ts = ts or datetime.now(timezone.utc).isoformat()
        appended = [{"message_id": message_id, "event": event, "ts": effective_ts}]
        with self._lock:
            self._validate(message_id, event, ts)
            if event == PROCESSED:
                if message_id in self._seen_processed:
                    appended.append(
                        {"message_id": message_id, "event": DUP_BUSINESS, "ts": effective_ts}
                    )
                else:
                    self._seen_processed.add(message_id)
            with self.path.open("a", encoding="utf-8") as f:
                for line in appended:
                    f.write(json.dumps(line, ensure_ascii=False) + "\n")
                f.flush()
        return appended


class _RecordHandler(BaseHTTPRequestHandler):
    recorder: Recorder  # 由 make_server 注入

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)  # 先消费 body（含 404 分支：为将来 keep-alive 留路）
        if self.path != "/records":
            self._send(404, {"error": "not found"})
            return
        try:
            payload = json.loads(body)
            message_id = payload["message_id"]
            event = payload["event"]
        except (ValueError, KeyError, TypeError):
            self._send(400, {"error": "invalid payload: expect {message_id, event[, ts]}"})
            return
        try:
            appended = self.recorder.record(message_id, event, payload.get("ts"))
        except ValueError as exc:  # 字段类型/空值（评审 I-1）
            self._send(400, {"error": str(exc)})
            return
        self._send(200, {"status": "recorded", "appended": appended})

    def _send(self, code: int, obj: dict) -> None:
        reply = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

    def log_message(self, format, *args):  # noqa: A002 -- 基类签名
        log.debug("%s - %s", self.address_string(), format % args)


def make_server(recorder: Recorder, host: str = "0.0.0.0", port: int = 8080) -> ThreadingHTTPServer:
    handler = type("BoundRecordHandler", (_RecordHandler,), {"recorder": recorder})
    return ThreadingHTTPServer((host, port), handler)


def serve(recorder: Recorder, host: str = "0.0.0.0", port: int = 8080) -> None:
    make_server(recorder, host, port).serve_forever()


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        prog="recorder", description="BRP result recorder (JSONL + DUP_BUSINESS)"
    )
    parser.add_argument("--port", type=int, default=8080)
    parser.add_argument("--jsonl-path", default="/data/records.jsonl")
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
    recorder = Recorder(args.jsonl_path)
    log.info("recorder listening on :%d, jsonl=%s", args.port, recorder.path)
    try:
        serve(recorder, port=args.port)
    except KeyboardInterrupt:
        log.info("recorder stopped")
    except OSError as exc:  # CLI 边界统一退出码 2（与前作 setup_broker/lab_factory 同口径）
        log.error("recorder failed: %s: %s", type(exc).__name__, exc)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
