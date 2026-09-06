#!/usr/bin/env python3
"""YAML 重复键查重（U6/LESSONS #8 的解析器侧防线）。

yaml.safe_load / compose config 对重复键一个静默 last-wins、一个报错措辞
难定位——本工具直接报出重复键的行号，把"看起来对"变成可审计的检查。

用法: python3 deploy/check-yaml-dups.py <file.yml> [file2.yml ...]
退出码: 0=无重复, 1=有重复或解析失败
"""
import sys
import yaml


class DupScanner(yaml.SafeLoader):
    pass


def scan(loader, node, deep=False):
    seen = {}
    for k_node, v_node in node.value:
        key = loader.construct_object(k_node, deep=deep)
        line = k_node.start_mark.line + 1
        if key in seen:
            print(f"DUPLICATE KEY '{key}': lines {seen[key]} and {line} ({node.start_mark.name})")
            scanner_fail.append(key)
        seen[key] = line
        loader.construct_object(v_node, deep=deep)
    return {}


scanner_fail = []
DupScanner.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, scan)

fail = False
for path in sys.argv[1:]:
    scanner_fail.clear()
    try:
        yaml.load(open(path), Loader=DupScanner)
    except yaml.YAMLError as e:
        print(f"{path}: PARSE ERROR: {e}")
        fail = True
        continue
    if scanner_fail:
        fail = True
    else:
        print(f"{path}: OK")

sys.exit(1 if fail else 0)
