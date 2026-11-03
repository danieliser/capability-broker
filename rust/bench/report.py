#!/usr/bin/env python3
import json, pathlib, statistics
root=pathlib.Path(__file__).resolve().parents[2];out=root/'rust/.bench'
data=json.loads((out/'results.json').read_text())
for kind,d in data.items():
 print(kind, 'median latency us',statistics.median(r['median_us'] for r in d['runs']), 'RSS MiB',d['idle_rss_kib']/1024,d['rss_after_2000_kib']/1024,'binary MiB',d['binary_bytes']/2**20,'cold ms',statistics.median(d['cold_start_ms']))
