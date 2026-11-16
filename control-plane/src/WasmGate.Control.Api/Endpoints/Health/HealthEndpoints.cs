using Npgsql;

namespace WasmGate.Control.Api.Endpoints.Health;

/// <summary>
/// Liveness and readiness probes.
/// </summary>
/// <remarks>
/// Liveness never touches the database: a restart does not fix an unavailable
/// database. Readiness does, so a load balancer stops sending traffic until
/// PostgreSQL answers again and resumes without a restart.
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

    /// <summary>Reports readiness: the process answers, PostgreSQL is reachable and the schema exists.</summary>
    /// <param name="dataSource">Connection pool for the control-plane database.</param>
    /// <param name="cancellationToken">Cancels the probe.</param>
    /// <returns><c>ok</c> when a trivial query against the schema succeeds, otherwise HTTP 503.</returns>
    /// <remarks>
    /// The probe reads from <c>agents</c> instead of running <c>SELECT 1</c>, so a
    /// process started before the migrations are applied reports 503 instead of
    /// looking healthy.
    /// </remarks>
    private static async Task<IResult> CheckReady(
        NpgsqlDataSource dataSource,
        CancellationToken cancellationToken)
    {
        try
        {
            await using var connection = await dataSource.OpenConnectionAsync(cancellationToken);
            await using var command = connection.CreateCommand();
            command.CommandText = "SELECT 1 FROM agents LIMIT 1";
            command.CommandTimeout = 2;
            await command.ExecuteScalarAsync(cancellationToken);
            return Results.Text("ok");
        }
        catch (NpgsqlException)
        {
            return Results.Text(
                "unavailable",
                statusCode: StatusCodes.Status503ServiceUnavailable);
        }
    }
}
