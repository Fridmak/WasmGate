using WasmGate.Control.Api.Endpoints.Agents;
using WasmGate.Control.Api.Endpoints.Artifacts;
using WasmGate.Control.Api.Endpoints.Audit;
using WasmGate.Control.Api.Endpoints.Backup;
using WasmGate.Control.Api.Endpoints.Environments;
using WasmGate.Control.Api.Endpoints.Health;
using WasmGate.Control.Api.Endpoints.Metrics;
using WasmGate.Control.Api.Endpoints.Releases;

namespace WasmGate.Control.Api.Endpoints.Admin;

/// <summary>
/// Composition of the admin HTTP API: health probes, artifact upload,
/// configuration versions, releases, deployments and rollback, agent
/// provisioning, environment catalog, registration tokens, Prometheus metrics
/// and the audit trail.
/// </summary>
/// <remarks>
/// Health probes stay public. The rest of the API is protected by the bearer
/// token when <c>WASMGATE_CP_ADMIN_TOKEN</c> is set; on a loopback or Unix
/// socket listener the token may be omitted. Because there is no authenticated
/// principal inside the handlers, every audit event written from these
/// endpoints uses the fixed actor <c>admin-api</c>.
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
            .MapEnvironmentEndpoints()
            .MapMetricsEndpoints()
            .MapAuditEndpoints()
            .MapBackupEndpoints();
}
