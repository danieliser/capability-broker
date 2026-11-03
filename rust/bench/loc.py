#!/usr/bin/env python3
"""Reproducible physical LOC; scoped Go sources are a superset, not an MVP."""
import json, pathlib
root=pathlib.Path(__file__).resolve().parents[2]
files=['rust/src/main.rs','pkg/mcpfront/front.go','pkg/adapters/upstream/upstream.go','pkg/connectors/connectors.go','pkg/audit/audit.go','pkg/durable/sqlite.go']
print(json.dumps({f:len((root/f).read_text().splitlines()) for f in files},indent=2))
