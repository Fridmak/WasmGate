# Benchmarks

Methodology, tables and interpretation:

- [English](../docs/en/benchmarks.md)
- [Русский](../docs/ru/benchmarks.md)

## Raw output

| File | Run |
|---|---|
| [`linux-ubuntu-results.txt`](linux-ubuntu-results.txt) | Historical pre-v4 run, 2026-11-05, commit `18d4249` |
| [`linux-current-results.txt`](linux-current-results.txt) | Current v4 run, 2026-11-19, stable `2ff37ac`, Linux container on Docker Desktop/WSL2, Go 1.27.4, 4 vCPU |
| [`windows-current-results.txt`](windows-current-results.txt) | Current v4 run, 2026-11-19, stable `2ff37ac`, Windows host, Go 1.27.1, 8 logical CPUs |

The current files are the source for the tables in the documentation. Each row
in those tables is the median of three `go test -bench` runs.

## Reproduce

The end-to-end proxy comparison needs Linux `splice(2)` and optionally nginx
(with the stream module) and HAProxy. On a Debian/Ubuntu host:

```bash
sudo apt-get install -y nginx libnginx-mod-stream haproxy
go test -run '^$' -bench . -benchmem -count=3 ./... | tee bench.txt
```

To compare two runs with `benchstat`:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
benchstat old.txt new.txt
```

The **Benchmarks** GitHub Actions workflow (`workflow_dispatch`) runs the same
command on a clean Ubuntu runner and uploads `bench-linux.txt`; GitHub runner
numbers are useful for relative comparisons only.
