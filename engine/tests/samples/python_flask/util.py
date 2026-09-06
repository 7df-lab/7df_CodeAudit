"""Helper module with an additional injected-command pattern."""
import subprocess


def resolve(hostname):
    # CWE-78 via subprocess concatenation (RuleScan RULE-CMD-001)
    return subprocess.check_output("nslookup " + hostname, shell=True)
