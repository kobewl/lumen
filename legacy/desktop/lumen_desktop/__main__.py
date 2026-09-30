"""支持 `python -m lumen_desktop` 方式运行。"""

import sys

from .cli import main

if __name__ == "__main__":
    sys.exit(main())
