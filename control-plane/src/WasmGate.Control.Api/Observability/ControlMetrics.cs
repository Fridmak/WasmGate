using System.Collections.Concurrent;
using System.Diagnostics;
using System.Globalization;
using System.Text;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Api.Observability;

/// <summary>
/// Process and control-plane metrics rendered in the Prometheus text format.
/// </summary>
/// <remarks>
/// The control plane deliberately avoids a metrics library: the surface is small,
/// the exposition format is stable, and a hand-written renderer keeps Native AOT
/// free of reflection and extra packages. The data plane uses the same format,
/// so one Prometheus instance can scrape both.
/// </remarks>
internal sealed class ControlMetrics
{
    /// <summary>Key of one HTTP counter sample.</summary>
    private readonly record struct HttpMetricKey(string Method, int Status);

    private readonly ConcurrentDictionary<HttpMetricKey, long> _http = new();
    private readonly long _startedAt = Stopwatch.GetTimestamp();

    /// <summary>Records one completed admin/API request.</summary>
    /// <param name="method">HTTP method.</param>
    /// <param name="status">Response status code.</param>
    public void RecordHttp(string method, int status)
    {
        // Bound the label set: clients may send arbitrary custom methods, and an
        // unbounded dictionary would grow once per spelling.
        _http.AddOrUpdate(
            new HttpMetricKey(NormalizeMethod(method), status),
            1,
            static (_, current) => current + 1);
    }

    /// <summary>Renders one Prometheus scrape.</summary>
    /// <param name="agents">Registered agents used for per-environment gauges.</param>
    /// <param name="activeSessions">Number of live gRPC agent sessions.</param>
    /// <returns>Prometheus text exposition 0.0.4.</returns>
    public string Render(IReadOnlyList<Agent> agents, int activeSessions)
    {
        ArgumentNullException.ThrowIfNull(agents);

        var builder = new StringBuilder(2048);
        var version = typeof(ControlMetrics).Assembly.GetName().Version?.ToString() ?? "dev";

        Gauge(builder, "wasmgate_control_build_info", "Control-plane build information.",
            $"version=\"{EscapeLabel(version)}\",dotnet_version=\"{EscapeLabel(Environment.Version.ToString())}\"", 1);

        builder.Append("# HELP wasmgate_control_http_requests_total Admin/API HTTP requests.\n");
        builder.Append("# TYPE wasmgate_control_http_requests_total counter\n");
        foreach (var sample in _http.OrderBy(pair => pair.Key.Method, StringComparer.Ordinal)
                                   .ThenBy(pair => pair.Key.Status))
        {
            builder.Append("wasmgate_control_http_requests_total{method=\"")
                .Append(EscapeLabel(sample.Key.Method))
                .Append("\",status=\"")
                .Append(sample.Key.Status.ToString(CultureInfo.InvariantCulture))
                .Append("\"} ")
                .Append(sample.Value.ToString(CultureInfo.InvariantCulture))
                .Append('\n');
        }

        builder.Append("# HELP wasmgate_control_agents Registered agents by environment and status.\n");
        builder.Append("# TYPE wasmgate_control_agents gauge\n");
        foreach (var group in agents
                     .GroupBy(agent => new { agent.Environment, agent.Status })
                     .OrderBy(group => group.Key.Environment, StringComparer.Ordinal)
                     .ThenBy(group => group.Key.Status))
        {
            builder.Append("wasmgate_control_agents{environment=\"")
                .Append(EscapeLabel(group.Key.Environment))
                .Append("\",status=\"")
                .Append(group.Key.Status)
                .Append("\"} ")
                .Append(group.Count().ToString(CultureInfo.InvariantCulture))
                .Append('\n');
        }

        Gauge(builder, "wasmgate_control_grpc_sessions_active",
            "Live agent control sessions.", null, activeSessions);
        Gauge(builder, "wasmgate_control_uptime_seconds",
            "Seconds since the control plane started.", null,
            Stopwatch.GetElapsedTime(_startedAt).TotalSeconds);
        Gauge(builder, "wasmgate_control_working_set_bytes",
            "Resident working set of the control-plane process.", null,
            Environment.WorkingSet);
        Gauge(builder, "wasmgate_control_gc_heap_bytes",
            "Managed heap size reported by GC.GetTotalMemory(false).", null,
            GC.GetTotalMemory(forceFullCollection: false));
        Gauge(builder, "wasmgate_control_thread_pool_threads",
            "Thread-pool thread count.", null, ThreadPool.ThreadCount);
        Counter(builder, "wasmgate_control_gen2_collections_total",
            "Gen2 garbage collections since process start.", null, GC.CollectionCount(2));

        return builder.ToString();
    }

    /// <summary>Appends one gauge sample with its HELP/TYPE block.</summary>
    private static void Gauge(StringBuilder builder, string name, string help, string? labels, double value)
    {
        builder.Append("# HELP ").Append(name).Append(' ').Append(help).Append('\n');
        builder.Append("# TYPE ").Append(name).Append(" gauge\n");
        builder.Append(name);
        if (!string.IsNullOrEmpty(labels))
        {
            builder.Append('{').Append(labels).Append('}');
        }
        builder.Append(' ').Append(Format(value)).Append('\n');
    }

    /// <summary>Appends one counter sample with its HELP/TYPE block.</summary>
    private static void Counter(StringBuilder builder, string name, string help, string? labels, long value)
    {
        builder.Append("# HELP ").Append(name).Append(' ').Append(help).Append('\n');
        builder.Append("# TYPE ").Append(name).Append(" counter\n");
        builder.Append(name);
        if (!string.IsNullOrEmpty(labels))
        {
            builder.Append('{').Append(labels).Append('}');
        }
        builder.Append(' ').Append(value.ToString(CultureInfo.InvariantCulture)).Append('\n');
    }

    /// <summary>Formats a floating-point sample without locale-dependent separators.</summary>
    private static string Format(double value) =>
        value.ToString("0.###", CultureInfo.InvariantCulture);

    /// <summary>Maps a request method to a bounded label value.</summary>
    private static string NormalizeMethod(string method)
    {
        var upper = method.ToUpperInvariant();
        return upper switch
        {
            "GET" or "HEAD" or "POST" or "PUT" or "PATCH" or "DELETE" or "OPTIONS" => upper,
            _ => "OTHER",
        };
    }

    /// <summary>Escapes a Prometheus label value.</summary>
    private static string EscapeLabel(string value) =>
        value.Replace("\\", "\\\\", StringComparison.Ordinal)
             .Replace("\"", "\\\"", StringComparison.Ordinal)
             .Replace("\n", "\\n", StringComparison.Ordinal);
}
