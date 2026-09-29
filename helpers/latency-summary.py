#!/usr/bin/env python3
"""Summarize latency-bench JSONL, keeping configurations and failures separate."""
import argparse
from collections import defaultdict
import json
import math
from pathlib import Path
import statistics


def summarize(records):
    groups = defaultdict(list)
    keys = ("mode", "audio_ms", "chunk_seconds", "pause_ms", "overlap_ms", "workers", "model", "language", "cleanup")
    for record in records:
        groups[tuple(record[key] for key in keys)].append(record)
    for key, runs in sorted(groups.items()):
        values = defaultdict(list)
        successful = [run for run in runs if run["result"] == "completed"]
        for run in successful:
            values["elapsed"].append(run["elapsed_ms"])
            if run["first_stt_ms"] is not None:
                values["first_stt"].append(run["first_stt_ms"])
            for stage, durations in run["timings_ms"].items():
                values[stage].extend(durations)
        yield {
            **dict(zip(keys, key)),
            "runs": len(runs),
            "failed": len(runs) - len(successful),
            "requests": [run["requests"] for run in runs],
            "max_active": max(run["max_active"] for run in runs),
            "latency_ms": {
                stage: {
                    "n": len(samples),
                    "median": statistics.median(samples),
                    "p95": sorted(samples)[math.ceil(0.95 * len(samples)) - 1],
                }
                for stage, samples in sorted(values.items()) if samples
            },
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("files", nargs="+", type=Path)
    args = parser.parse_args()
    records = [json.loads(line) for path in args.files for line in path.read_text().splitlines() if line.strip()]
    for summary in summarize(records):
        print(json.dumps(summary, ensure_ascii=False))


if __name__ == "__main__":
    main()
