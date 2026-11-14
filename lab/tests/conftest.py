"""lab 测试共享：把仓库根加入 sys.path，使 `lab.helpers.*` 可导入。"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
