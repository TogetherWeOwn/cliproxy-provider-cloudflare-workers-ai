#!/usr/bin/env python3
"""Supplement gitleaks with repository-publication checks; print no matched data."""
import pathlib
import re
import sys

RULES = {
    "private-key": re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    "credential-token": re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-[A-Za-z0-9_-]{24,})\b"),
    "personal-email": re.compile(r"\b[A-Za-z0-9_.+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b"),
    "private-address": re.compile(r"\b(?:10\.\d+\.\d+\.\d+|192\.168\.\d+\.\d+|172\.(?:1[6-9]|2\d|3[01])\.\d+\.\d+)\b"),
    "internal-work-item": re.compile(r"\b(?!SHA-)[A-Z]{3}-[0-9]+\b"),
    "private-home-path": re.compile(r"/home/[a-zA-Z0-9_-]+/"),
}


def scan(root):
    failures = []
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root)
        if any(part in {".git", "dist", "__pycache__"} for part in relative.parts):
            continue
        if path.is_symlink():
            failures.append(f"{relative}: symlink is not a public source file")
            continue
        if not path.is_file():
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            failures.append(f"{relative}: unexpected binary source file")
            continue
        for line_number, line in enumerate(text.splitlines(), 1):
            for name, pattern in RULES.items():
                if pattern.search(line):
                    failures.append(f"{relative}:{line_number}: {name}")
    return failures


if __name__ == "__main__":
    issues = scan(pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve())
    for issue in issues:
        print(issue, file=sys.stderr)
    if issues:
        sys.exit(1)
    print("Public-file checks passed")
