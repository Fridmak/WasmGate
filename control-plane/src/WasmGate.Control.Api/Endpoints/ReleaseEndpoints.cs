using System.Text;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>
/// Configuration, release and deployment endpoints: the publish path from a YAML
/// document to a command on an agent.
/// </summary>
internal static class ReleaseEndpoints
{
    /// <summary>Upper bound for the number of artifacts in one release.</summary>
    private const int MaxReleaseArtifacts = 64;

    /// <summary>Maps the config, release and deployment routes.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapReleaseEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapPost("/api/v1/configs", CreateConfig);
        app.MapGet("/api/v1/configs/{environment}", ListConfigs);
        app.MapPost("/api/v1/releases", CreateRelease);
        app.MapGet("/api/v1/releases/{environment}", ListReleases);
        app.MapPost("/api/v1/releases/{id:guid}/revoke", RevokeRelease);
        app.MapPost("/api/v1/deployments", CreateDeployment);
        app.MapGet("/api/v1/deployments/{environment}", ListDeployments);
        app.MapPost("/api/v1/deployments/{id:guid}/rollback", Rollback);
        return app;
    }

    /// <summary>Stores an immutable configuration version.</summary>
    /// <param name="request">Environment, YAML and an optional message.</param>
    /// <param name="service">Release service.</param>
    /// <param name="options">Limits applied to the document size.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The version id and hash; an unchanged YAML returns the existing version.</returns>
    private static async Task<IResult> CreateConfig(
        CreateConfigRequest request,
        IReleaseService service,
        ControlPlaneOptions options,
        CancellationToken cancellationToken)
    {
        if (Encoding.UTF8.GetByteCount(request.Yaml ?? string.Empty) > options.MaxConfigBytes)
        {
            return Results.BadRequest(
                $"Configuration document exceeds the {options.MaxConfigBytes} byte limit.");
        }

        try
        {
            var config = await service.CreateConfigAsync(
                request.Environment,
                request.Yaml ?? string.Empty,
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
    /// <returns>The release id and its manifest signature.</returns>
    private static async Task<IResult> CreateRelease(
        CreateReleaseRequest request,
        IReleaseService service,
        CancellationToken cancellationToken)
    {
        if (request.Artifacts is { Count: > MaxReleaseArtifacts })
        {
            return Results.BadRequest(
                $"A release may contain at most {MaxReleaseArtifacts} artifacts.");
        }

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

    /// <summary>Marks a release as revoked so it can no longer be deployed.</summary>
    /// <param name="id">Release identifier.</param>
    /// <param name="releases">Release repository, used to resolve the environment.</param>
    /// <param name="service">Release service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>204 on success, 404 for an unknown release.</returns>
    private static async Task<IResult> RevokeRelease(
        Guid id,
        IReleaseRepository releases,
        IReleaseService service,
        CancellationToken cancellationToken)
    {
        var release = await releases.FindAsync(id, cancellationToken);
        if (release is null)
        {
            return Results.NotFound();
        }

        try
        {
            await service.RevokeAsync(release.Environment, id, actor: "admin-api", cancellationToken);
            return Results.NoContent();
        }
        catch (InvalidOperationException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists the 100 most recent deployments of an environment.</summary>
    /// <param name="environment">Environment name.</param>
    /// <param name="deployments">Deployment repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListDeployments(
        string environment,
        IDeploymentRepository deployments,
        CancellationToken cancellationToken) =>
        Results.Ok(await deployments.ListAsync(environment, 100, cancellationToken));

    /// <summary>Starts a deployment and returns its initial status.</summary>
    /// <param name="request">Release and wave selectors.</param>
    /// <param name="service">Release service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The deployment id and its status after the first dispatch.</returns>
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
    /// <returns>204 on success, 404 for an unknown deployment, 400 when it cannot be rolled back.</returns>
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
}
