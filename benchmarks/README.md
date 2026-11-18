# Benchmarks

Methodology, results and interpretation:

- [English](../docs/en/benchmarks.md)
- [Русский](../docs/ru/benchmarks.md)

This directory keeps the raw output of the comparison runs, for example
[`linux-ubuntu-results.txt`](linux-ubuntu-results.txt). That file was captured
on 2026-11-05 at commit `18d4249`, before the ABI v4 cutover, and is historical.

Reproduce with `make bench`, or run the **Benchmarks** GitHub Actions workflow
(`workflow_dispatch`) and download the `bench-linux` artifact.
