"""事件词汇契约（单一事实源，M1 第 23 天收口）：producer/consumer/recorder/
conftest/scenarios/agent.reconcile.counter 全部 import 此处，**勿改名**。

- 六事件词：五阶段口径（实施规划 §5）+ recorder 派生词 DUP_BUSINESS；
- PROBE_PREFIX：烟雾/就绪探针的 message_id 前缀，一切计数侧一律排除（day21 契约）。
"""

# 事件词汇契约（第 20/22/23 天 producer/consumer/场景/counter 共用，勿改名）：
EVENT_SEND = "SEND"
EVENT_CONFIRM = "CONFIRM"
EVENT_DELIVERED = "DELIVERED"
EVENT_PROCESSED = "PROCESSED"
EVENT_ACKED = "ACKED"
EVENT_DUP_BUSINESS = "DUP_BUSINESS"

# 探针前缀：写入共享卷的烟雾/就绪探针行，计数契约一律排除（day21 I-1）
PROBE_PREFIX = "fixture-probe-"
