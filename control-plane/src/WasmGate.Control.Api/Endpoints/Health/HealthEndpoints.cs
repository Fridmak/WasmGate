using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Api.Endpoints.Health;
/// <summary>
/// Liveness and readiness probes.
/// </summary>
/// <remarks>
/// Liveness never touches the database: a restart does not fix an unavailable
/// database. Readiness does, so a load balancer stops sending traffic until the
/// selected store answers again and resumes without a restart.
/// </remarks>
internal static class HealthEndpoints
{
    /// <summary>Maps <c>/healthz</c> and <c>/readyz</c>.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapHealthEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapGet("/healthz", () => Results.Text("ok"));
        app.MapGet("/readyz", CheckReady);
        return app;
    }

    /// <summary>Reports readiness: the process answers and the storage schema is reachable.</summary>
    /// <param name="probe">SQLite readiness probe.</param>
    /// <param name="cancellationToken">Cancels the probe.</param>
    /// <returns><c>ok</c> when a trivial query against the schema succeeds, otherwise HTTP 503.</returns>
    private static async Task<IResult> CheckReady(
        SqliteStorageProbe probe,
        CancellationToken cancellationToken)
    {
        try
        {
            return await probe.CheckAsync(cancellationToken)
                ? Results.Text("ok")
                : Results.Text(
                    "unavailable",
                    statusCode: StatusCodes.Status503ServiceUnavailable);
        }
        catch (OperationCanceledException)
        {
            throw;
        }
        catch (Exception)
        {
            return Results.Text(
                "unavailable",
                statusCode: StatusCodes.Status503ServiceUnavailable);
        }
    }
}
