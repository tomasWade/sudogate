#!/usr/bin/env python3
import json
import os
import shutil
import sys

WIDGET_ID = "tomaswade.sudogate"

path = os.path.expanduser("~/.config/omarchy/shell.json")
if not os.path.exists(path):
    sys.exit(f"未找到 {path}，请确认 omarchy 环境后再运行")

with open(path, encoding="utf-8") as f:
    data = json.load(f)

right = data.setdefault("layout", {}).setdefault("right", [])
if WIDGET_ID in right:
    print(f"{WIDGET_ID} 已注册于 layout.right，跳过")
    sys.exit(0)

shutil.copyfile(path, path + ".bak-sudogate")
right.append(WIDGET_ID)
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, ensure_ascii=False, indent=2)
    f.write("\n")
print(f"已追加 {WIDGET_ID} 到 layout.right（备份: shell.json.bak-sudogate）")
