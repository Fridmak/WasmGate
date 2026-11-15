using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>
/// Admin HTTP API. The routes implemented here cover the complete MVP path:
/// artifact upload, configuration versions, releases, deployments, rollback,
/// agent provisioning and certificate onboarding.
/// </summary>
public static class AdminEndpoints
{
    /// <summary>Maps the administrative and health endpoints.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapAdminEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapGet("/healthz", () => Results.Text("ok"));
        app.MapGet("/readyz", CheckReady);

        app.MapPost("/api/v1/artifacts", UploadArtifact);
        app.MapPost("/api/v1/configs", CreateConfig);
        app.MapGet("/api/v1/configs/{environment}", ListConfigs);
        app.MapPost("/api/v1/releases", CreateRelease);
        app.MapGet("/api/v1/releases/{environment}", ListReleases);
        app.MapPost("/api/v1/deployments", CreateDeployment);
        app.MapPost("/api/v1/deployments/{id:guid}/rollback", Rollback);
        app.MapPost("/api/v1/agents", CreateAgent);
        app.MapGet("/api/v1/agents", ListAgents);
        app.MapPost("/api/v1/registration-tokens", CreateRegistrationToken);
        app.MapGet("/api/v1/audit", ListAudit);

        return app;
    }

    /// <summary>Stores an artifact and its metadata.</summary>
    /// <param name="request">Artifact bytes and metadata.</param>
    /// <param name="store">Content-addressed artifact store.</param>
    /// <param name="repository">Artifact metadata repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> UploadArtifact(
        UploadArtifactRequest request,
        IArtifactStore store,
        IArtifactRepository repository,
        CancellationToken cancellationToken)
    {
        byte[] content;
        try
        {
            content = Convert.FromBase64String(request.ContentBase64);
        }
        catch (FormatException)
        {
            return Results.BadRequest("ContentBase64 is not valid base64.");
        }

        await using var stream = new MemoryStream(content, writable: false);
        var artifact = await store.PutAsync(
            stream,
            request.Kind,
            request.MediaType,
            createdBy: "admin-api",
            cancellationToken);
        await repository.AddAsync(artifact, cancellationToken);
        return Results.Ok(new UploadArtifactResponse(artifact.Id, artifact.Size));
    }

    /// <summary>Creates an immutable configuration version.</summary>
    /// <param name="request">Environment and YAML.</param>
    /// <param name="service">Release service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> CreateConfig(
        CreateConfigRequest request,
        IReleaseService service,
        CancellationToken cancellationToken)
    {
        try
        {
            var config = await service.CreateConfigAsync(
                request.Environment,
                request.Yaml,
                actor: "admin-api",
                request.Message,
                cancellationToken);
            return Results.Ok(new CreateConfigResponse(config.Id, config.Sha256));
        }
        catch (ArgumentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists configuration versions for an environment.</summary>
    /// <param name="environment">Environment name.</param>
    /// <param name="repository">Configuration repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListConfigs(
        string environment,
        IConfigVersionRepository repository,
        CancellationToken cancellationToken) =>
        Results.Ok(await repository.ListAsync(environment, cancellationToken));

    /// <summary>Creates a validated and signed release.</summary>
    /// <param name="request">Configuration version and artifacts.</param>
    /// <param name="service">Release service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> CreateRelease(
        CreateReleaseRequest request,
        IReleaseService service,
        CancellationToken cancellationToken)
    {
        try
        {
            var release = await service.CreateReleaseAsync(
                request.Environment,
                request.ConfigVersionId,
                (request.Artifacts ?? [])
                    .Select(a => new ReleaseArtifact { ArtifactId = a.ArtifactId, Name = a.Name })
                    .ToList(),
                actor: "admin-api",
                cancellationToken);
            return Results.Ok(new CreateReleaseResponse(release.Id, release.Signature));
        }
        catch (InvalidOperationException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists releases for an environment.</summary>
    /// <param name="environment">Environment name.</param>
    /// <param name="repository">Release repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListReleases(
        string environment,
        IReleaseRepository repository,
        CancellationToken cancellationToken) =>
        Results.Ok(await repository.ListAsync(environment, cancellationToken));

    /// <summary>Starts a deployment across ordered waves.</summary>
    /// <param name="request">Release and wave selectors.</param>
    /// <param name="service">Release service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> CreateDeployment(
        CreateDeploymentRequest request,
        IReleaseService service,
        CancellationToken cancellationToken)
    {
        try
        {
            var deployment = await service.DeployAsync(
                request.Environment,
                request.ReleaseId,
                request.WaveSelectors ?? [],
                actor: "admin-api",
                cancellationToken);
            return Results.Ok(new CreateDeploymentResponse(deployment.Id, deployment.Status));
        }
        catch (InvalidOperationException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Rolls a deployment back to the previous release.</summary>
    /// <param name="id">Deployment identifier.</param>
    /// <param name="deployments">Deployment repository.</param>
    /// <param name="service">Release service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> Rollback(
        Guid id,
        IDeploymentRepository deployments,
        IReleaseService service,
        CancellationToken cancellationToken)
    {
        var deployment = await deployments.FindAsync(id, cancellationToken);
        if (deployment is null)
        {
            return Results.NotFound();
        }

        try
        {
            await service.RollbackAsync(
                deployment.Environment,
                id,
                actor: "admin-api",
                cancellationToken);
            return Results.NoContent();
        }
        catch (InvalidOperationException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Creates one agent and returns a one-time enrollment token.</summary>
    /// <param name="request">Agent name and labels.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> CreateAgent(
        CreateAgentRequest request,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            var result = await provisioning.CreateAgentAsync(
                request.Name,
                request.Labels ?? new Dictionary<string, string>(),
                actor: "admin-api",
                cancellationToken);

            return Results.Ok(new CreateAgentResponse(result.AgentId, result.Token));
        }
        catch (EnrollmentException ex) when (ex.Failure == EnrollmentFailure.AlreadyExists)
        {
            return Results.Conflict(ex.Message);
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists registered agents.</summary>
    /// <param name="agents">Agent repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListAgents(
        IAgentRepository agents,
        CancellationToken cancellationToken)
    {
        var list = await agents.ListAsync(cancellationToken);
        return Results.Ok(list
            .Select(a => new AgentSummary(a.Id, a.Name, a.Status, a.LastSeenAt, a.ActiveReleaseId))
            .ToList());
    }

    /// <summary>Creates a registration token that many agents can share.</summary>
    /// <param name="request">Token scope and limits.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> CreateRegistrationToken(
        CreateRegistrationTokenRequest request,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            var result = await provisioning.CreateRegistrationTokenAsync(
                request.NamePrefix,
                request.MaxUses,
                TimeSpan.FromSeconds(request.TtlSeconds),
                actor: "admin-api",
                cancellationToken);

            return Results.Ok(new CreateRegistrationTokenResponse(result.Token, result.MaxUses, result.ExpiresAt));
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists recent audit events.</summary>
    /// <param name="audit">Audit log.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListAudit(
        IAuditLog audit,
        CancellationToken cancellationToken) =>
        Results.Ok(await audit.ListAsync(100, cancellationToken));

    /// <summary>Reports readiness: the process answers and PostgreSQL is reachable.</summary>
    /// <param name="dataSource">Connection pool for the control-plane database.</param>
    /// <param name="cancellationToken">Cancels the probe.</param>
    /// <returns><c>ok</c> when a trivial query succeeds, otherwise HTTP 503.</returns>
    private static async Task<IResult> CheckReady(
        NpgsqlDataSource dataSource,
        CancellationToken cancellationToken)
    {
        try
        {
            await using var connection = await dataSource.OpenConnectionAsync(cancellationToken);
            await using var command = connection.CreateCommand();
            command.CommandText = "SELECT 1";
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
