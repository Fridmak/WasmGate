using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Core.Abstractions.Releases;

/// <summary>
/// Release workflow: immutable configuration versions, validated and signed
/// releases, wave-based deployment and rollback.
/// </summary>
public interface IReleaseService
{
    /// <summary>Stores an immutable configuration version.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="yaml">Raw YAML configuration.</param>
    /// <param name="actor">Identity of the principal creating the version.</param>
    /// <param name="message">Optional change description.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The stored version, or the existing one when the YAML is unchanged.</returns>
    /// <exception cref="ArgumentException">The environment or YAML is empty or the environment name is invalid.</exception>
    Task<ConfigVersion> CreateConfigAsync(
        string environment,
        string yaml,
        string actor,
        string? message,
        CancellationToken cancellationToken);

    /// <summary>Creates a validated and signed release.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="configVersionId">Configuration version included in the release.</param>
    /// <param name="artifacts">Artifacts with their install paths; paths must be unique.</param>
    /// <param name="actor">Identity of the principal creating the release.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The stored release with its signature.</returns>
    /// <remarks>
    /// The configuration and the artifacts are validated by the Go data plane
    /// before the release is stored, so an invalid WASM module never reaches an
    /// agent.
    /// </remarks>
    /// <exception cref="InvalidOperationException">
    /// The configuration or an artifact is missing, belongs to another
    /// environment, or fails validation.
    /// </exception>
    Task<Release> CreateReleaseAsync(
        string environment,
        Guid configVersionId,
        IReadOnlyList<ReleaseArtifact> artifacts,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Starts a deployment of one release.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="releaseId">Release to deploy.</param>
    /// <param name="waveSelectors">
    /// One JSON label selector per wave, for example <c>{"role":"canary"}</c>;
    /// an empty list means a single wave that targets every agent.
    /// </param>
    /// <param name="actor">Identity of the principal starting the deployment.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The created deployment.</returns>
    /// <remarks>
    /// Commands are sent only to agents with a live session that belong to
    /// <paramref name="environment"/>. Waves are recorded and dispatched in
    /// order, but a wave does not wait for the previous one to finish; the
    /// deployment turns terminal when the first agent reports a result. The
    /// release signature is verified before dispatch.
    /// </remarks>
    /// <exception cref="InvalidOperationException">
    /// The release is missing, revoked, belongs to another environment, is
    /// unsigned, its signature does not match the manifest, an artifact of the
    /// release is missing, or a selector is not a JSON object of string values.
    /// </exception>
    Task<Deployment> DeployAsync(
        string environment,
        Guid releaseId,
        IReadOnlyList<string> waveSelectors,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Rolls a deployment back to the previous release of the environment.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="deploymentId">Deployment to roll back.</param>
    /// <param name="actor">Identity of the principal performing the rollback.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Marks the deployment RolledBack and then deploys the previous release with
    /// the same wave selectors, so agents receive it as a normal ApplyRelease.
    /// The previous release is the newest older release of the same environment
    /// that was not revoked.
    /// </remarks>
    /// <exception cref="InvalidOperationException">
    /// The deployment is missing, already rolled back, belongs to another
    /// environment, or there is no earlier release to return to.
    /// </exception>
    Task RollbackAsync(
        string environment,
        Guid deploymentId,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Marks a release as revoked so it can no longer be deployed.</summary>
    /// <param name="environment">Logical environment name.</param>
    /// <param name="releaseId">Release to revoke.</param>
    /// <param name="actor">Identity of the principal revoking the release.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Revocation is a tombstone: the release row and its artifacts stay for
    /// audit, but <see cref="DeployAsync"/> rejects it. Releases already applied
    /// to an agent are not rolled back automatically.
    /// </remarks>
    /// <exception cref="InvalidOperationException">
    /// The release is missing or belongs to another environment.
    /// </exception>
    Task RevokeAsync(
        string environment,
        Guid releaseId,
        string actor,
        CancellationToken cancellationToken);
}
