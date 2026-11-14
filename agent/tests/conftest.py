"""agent 测试共享：把 agent/ 目录加入 sys.path，使 `envmgr.*` 可导入。"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
