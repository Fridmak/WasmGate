using WasmGate.Control.Core.Abstractions.Repositories;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>Audit trail endpoint.</summary>
internal static class AuditEndpoints
{
    /// <summary>Maps <c>GET /api/v1/audit</c>.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapAuditEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapGet("/api/v1/audit", ListAudit);
        return app;
    }

    /// <summary>Lists the 100 most recent audit events.</summary>
    /// <param name="audit">Audit log.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListAudit(
        IAuditLog audit,
        CancellationToken cancellationToken) =>
        Results.Ok(await audit.ListAsync(100, cancellationToken));
}
