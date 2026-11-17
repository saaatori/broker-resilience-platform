"""agent 测试共享：把 agent/ 与仓库根加入 sys.path（envmgr.* 与 lab.helpers.* 可导入）。"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))  # agent/
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))  # 仓库根（lab 包）
