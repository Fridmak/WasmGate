using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>
/// High-level release workflow. Kept as an interface so the HTTP API, CLI and
/// future CI integration share one implementation.
/// </summary>
public interface IReleaseService
{
    /// <summary>Creates a new immutable configuration version.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="yaml">Raw YAML configuration.</param>
    /// <param name="actor">Identity of the principal creating the version.</param>
    /// <param name="message">Optional change description.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The stored configuration version.</returns>
    Task<ConfigVersion> CreateConfigAsync(
        string environment,
        string yaml,
        string actor,
        string? message,
        CancellationToken cancellationToken);

    /// <summary>Creates a release from a validated configuration and artifacts.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="configVersionId">Configuration version included in the release.</param>
    /// <param name="artifacts">Artifacts and their install paths.</param>
    /// <param name="actor">Identity of the principal creating the release.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The stored release.</returns>
    Task<Release> CreateReleaseAsync(
        string environment,
        Guid configVersionId,
        IReadOnlyList<ReleaseArtifact> artifacts,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Starts a deployment of one release across ordered waves.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="releaseId">Release to deploy.</param>
    /// <param name="waveSelectors">Raw JSON label selectors, one per wave.</param>
    /// <param name="actor">Identity of the principal starting the deployment.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The created deployment.</returns>
    Task<Deployment> DeployAsync(
        string environment,
        Guid releaseId,
        IReadOnlyList<string> waveSelectors,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Rolls a deployment back to the previous release.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="deploymentId">Deployment to roll back.</param>
    /// <param name="actor">Identity of the principal performing the rollback.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task RollbackAsync(
        string environment,
        Guid deploymentId,
        string actor,
        CancellationToken cancellationToken);
}
