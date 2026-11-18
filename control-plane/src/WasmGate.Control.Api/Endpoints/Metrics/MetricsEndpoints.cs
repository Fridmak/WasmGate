using WasmGate.Control.Api.Observability;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Api.Endpoints.Metrics;

/// <summary>Prometheus scrape endpoint for the control plane.</summary>
internal static class MetricsEndpoints
{
    /// <summary>Maps <c>GET /metrics</c>.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapMetricsEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapGet("/metrics", WriteMetrics);
        return app;
    }

    /// <summary>Renders current control-plane metrics.</summary>
    /// <param name="agents">Agent repository used for environment/status gauges.</param>
    /// <param name="sessions">Live session registry.</param>
    /// <param name="metrics">Counter store.</param>
    /// <param name="logger">Logger for database failures.</param>
    /// <param name="cancellationToken">Cancels the database query.</param>
    private static async Task<IResult> WriteMetrics(
        IAgentRepository agents,
        IAgentSessionRegistry sessions,
        ControlMetrics metrics,
        ILogger<ControlMetrics> logger,
        CancellationToken cancellationToken)
    {
        IReadOnlyList<Agent> list;
        using var dbTimeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        // A scrape must return even when SQLite is slow or unreachable:
        // Prometheus gives up after its own scrape timeout and marks the target
        // down, hiding the process metrics that are still perfectly valid.
        dbTimeout.CancelAfter(TimeSpan.FromSeconds(2));
        try
        {
            list = await agents.ListAsync(dbTimeout.Token);
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            logger.LogWarning(ex, "agent metrics unavailable; rendering process metrics only");
            metrics.RecordAgentQueryFailure();
            list = [];
        }

        return Results.Text(
            metrics.Render(list, sessions.Count),
            "text/plain; version=0.0.4; charset=utf-8");
    }
}
