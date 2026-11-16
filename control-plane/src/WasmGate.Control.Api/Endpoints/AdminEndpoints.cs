namespace WasmGate.Control.Api.Endpoints;

/// <summary>
/// Composition of the admin HTTP API: health probes, artifact upload,
/// configuration versions, releases, deployments and rollback, agent
/// provisioning, registration tokens and the audit trail.
/// </summary>
/// <remarks>
/// The API has no authentication yet. It is meant to stay on a loopback or
/// private address and must not be exposed to the internet. Because there is no
/// authenticated principal, every audit event written from these endpoints uses
/// the fixed actor <c>admin-api</c>.
/// </remarks>
internal static class AdminEndpoints
{
    /// <summary>Maps every administrative and health endpoint.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapAdminEndpoints(this IEndpointRouteBuilder app) =>
        app
            .MapHealthEndpoints()
            .MapArtifactEndpoints()
            .MapReleaseEndpoints()
            .MapAgentEndpoints()
            .MapAuditEndpoints();
}
