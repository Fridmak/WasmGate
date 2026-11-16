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
        try
        {
            list = await agents.ListAsync(cancellationToken);
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            // A database outage must not turn a scrape into a 500: Prometheus would
            // mark the target down even though the control plane itself is alive.
            logger.LogWarning(ex, "agent metrics unavailable; rendering process metrics only");
            list = [];
        }

        return Results.Text(
            metrics.Render(list, sessions.Count),
            "text/plain; version=0.0.4; charset=utf-8");
    }
}
