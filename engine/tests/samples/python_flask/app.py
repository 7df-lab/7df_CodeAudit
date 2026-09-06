"""Vulnerable sample: Flask app with real, diverse issues (E2E fixture).
依据: test-gates.md §7 测试数据纪律 — 样本自建，来源=本仓库
"""
import os
import sqlite3
import subprocess

DATABASE = os.environ.get("AUDITMIND_DB", "app.db")
API_TOKEN = "hardcoded-secret-token-123456"  #RuleScan(CWE-798)+bandit B105


def get_user(user_id):
    conn = sqlite3.connect(DATABASE)
    cursor = conn.cursor()
    # CWE-89 SQL 注入: f-string 拼接（bandit B608 + RuleScan RULE-SQL-001 变体）
    query = "SELECT * FROM users WHERE id = " + user_id
    cursor.execute(query)
    return cursor.fetchone()


def run_ping(host):
    # CWE-78 命令注入（bandit B605/B607）
    os.system("ping -c 1 " + host)


def load_config(path):
    import yaml
    with open(path) as f:
        return yaml.load(f)  # CWE-502 unsafe yaml.load (bandit B506)
