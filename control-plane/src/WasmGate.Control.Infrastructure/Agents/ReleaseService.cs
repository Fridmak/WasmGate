using System.Security.Cryptography;
using System.Text;
using Microsoft.Extensions.Logging;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Security;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Release workflow: immutable configuration versions, validation by the Go data
/// plane, signing, wave dispatch and rollback.
/// </summary>
/// <remarks>
/// There is no health gate and no approval step. Waves are dispatched in order
/// without waiting for the previous one, and the first apply result reported by
/// an agent turns the deployment and all of its dispatched waves terminal.
/// Before dispatch the stored manifest signature is verified, so a release that
/// was altered in the database is never sent to an agent.
/// </remarks>
public sealed class ReleaseService(
    IConfigVersionRepository configs,
    IArtifactRepository artifactRepository,
    IReleaseRepository releases,
    IDeploymentRepository deployments,
    IAgentRepository agents,
    IAgentSessionRegistry sessions,
    IConfigValidator validator,
    IReleaseSigner signer,
    IAuditLog audit,
    ControlPlaneOptions options,
    ILogger<ReleaseService> logger) : IReleaseService
{
    /// <inheritdoc />
    public async Task<ConfigVersion> CreateConfigAsync(
        string environment,
        string yaml,
        string actor,
        string? message,
        CancellationToken cancellationToken)
    {
        if (string.IsNullOrWhiteSpace(environment))
        {
            throw new ArgumentException("Environment is required.", nameof(environment));
        }
        if (string.IsNullOrWhiteSpace(yaml))
        {
            throw new ArgumentException("Configuration body is required.", nameof(yaml));
        }

        var normalizedEnvironment = environment.Trim();
        var sha = Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(yaml))).ToLowerInvariant();
        var existing = await configs.FindByHashAsync(normalizedEnvironment, sha, cancellationToken);
        if (existing is not null)
        {
            return existing;
        }

        var version = new ConfigVersion
        {
            Environment = normalizedEnvironment,
            Sha256 = sha,
            Yaml = yaml,
            Message = message,
            CreatedBy = actor,
        };
        await configs.AddAsync(version, cancellationToken);
        await AuditAsync(actor, "config.create", version.Id.ToString("D"), cancellationToken);
        return version;
    }

    /// <inheritdoc />
    public async Task<Release> CreateReleaseAsync(
        string environment,
        Guid configVersionId,
        IReadOnlyList<ReleaseArtifact> artifacts,
        string actor,
        CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(artifacts);

        var config = await configs.FindAsync(configVersionId, cancellationToken)
                     ?? throw new InvalidOperationException("Configuration version not found.");

        if (!string.Equals(config.Environment, environment, StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException(
                $"Configuration version belongs to environment '{config.Environment}', not '{environment}'.");
        }

        var names = new HashSet<string>(StringComparer.Ordinal);
        foreach (var artifact in artifacts)
        {
            if (string.IsNullOrWhiteSpace(artifact.Name))
            {
                throw new InvalidOperationException("Artifact install path is required.");
            }

            if (!names.Add(artifact.Name))
            {
                throw new InvalidOperationException($"Artifact path '{artifact.Name}' is listed more than once.");
            }

            if (await artifactRepository.FindAsync(artifact.ArtifactId, cancellationToken) is null)
            {
                throw new InvalidOperationException($"Artifact '{artifact.ArtifactId}' not found.");
            }
        }

        var validation = await validator.ValidateAsync(config, artifacts, cancellationToken);
        if (!validation.IsValid)
        {
            throw new InvalidOperationException(
                "Release validation failed: " +
                string.Join("; ", validation.Issues.Where(i => i.IsError).Select(i => i.Message)));
        }

        var release = new Release
        {
            Environment = environment,
            ConfigVersionId = config.Id,
            ConfigSha256 = config.Sha256,
            Artifacts = artifacts,
            Status = ReleaseStatus.Validated,
            CreatedBy = actor,
        };
        release.Signature = await signer.SignAsync(release, cancellationToken);
        await releases.AddAsync(release, cancellationToken);
        await AuditAsync(actor, "release.create", release.Id.ToString("D"), cancellationToken);
        return release;
    }

    /// <inheritdoc />
    public async Task<Deployment> DeployAsync(
        string environment,
        Guid releaseId,
        IReadOnlyList<string> waveSelectors,
        string actor,
        CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(waveSelectors);

        var release = await releases.FindAsync(releaseId, cancellationToken)
                      ?? throw new InvalidOperationException("Release not found.");
        if (release.Status == ReleaseStatus.Revoked)
        {
            throw new InvalidOperationException("Release is revoked.");
        }

        if (!string.Equals(release.Environment, environment, StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException(
                $"Release belongs to environment '{release.Environment}', not '{environment}'.");
        }

        await EnsureSignatureAsync(release, cancellationToken);

        var parsedSelectors = waveSelectors.Count == 0
            ? new List<WaveSelector> { WaveSelector.Parse(WaveSelector.All) }
            : waveSelectors.Select(WaveSelector.Parse).ToList();

        var config = await configs.FindAsync(release.ConfigVersionId, cancellationToken)
                     ?? throw new InvalidOperationException("Configuration version not found.");

        var deployment = new Deployment
        {
            Environment = environment,
            ReleaseId = releaseId,
            CreatedBy = actor,
            Status = DeploymentStatus.Running,
            Waves = waveSelectors.Count == 0
                ? [new DeploymentWave { Index = 0, Selector = WaveSelector.All }]
                : waveSelectors
                    .Select((selector, index) => new DeploymentWave { Index = index, Selector = selector })
                    .ToList(),
        };
        await deployments.AddAsync(deployment, cancellationToken);

        var allAgents = await agents.ListAsync(cancellationToken);
        var artifactRefs = new List<AgentArtifact>();
        foreach (var reference in release.Artifacts)
        {
            var artifact = await artifactRepository.FindAsync(reference.ArtifactId, cancellationToken)
                           ?? throw new InvalidOperationException(
                               $"Artifact '{reference.ArtifactId}' of the release is missing from the artifact repository.");

            artifactRefs.Add(new AgentArtifact(
                artifact.Id,
                reference.Name,
                artifact.Kind,
                artifact.Size,
                artifact.Signature));
        }

        var commandsSent = 0;

        foreach (var wave in deployment.Waves)
        {
            var targets = allAgents.Where(agent => parsedSelectors[wave.Index].Matches(agent)).ToList();
            if (targets.Count == 0)
            {
                wave.Status = DeploymentWaveStatus.Skipped;
                continue;
            }

            wave.Status = DeploymentWaveStatus.Running;
            wave.StartedAt = DateTimeOffset.UtcNow;

            var delivered = 0;
            foreach (var agent in targets)
            {
                if (!sessions.TryGet(agent.Id, out var session))
                {
                    continue;
                }

                try
                {
                    await session.SendAsync(
                        new AgentCommand(
                            AgentCommandKind.ApplyRelease,
                            ReleaseId: release.Id.ToString("D"),
                            Reason: "deployment",
                            TimeoutMs: ApplyTimeoutMs(),
                            ConfigSha256: config.Sha256,
                            ConfigYaml: config.Yaml,
                            Artifacts: artifactRefs,
                            DeploymentId: deployment.Id.ToString("D"),
                            Manifest: ReleaseManifest.Canonicalize(release),
                            ManifestSignature: Convert.FromBase64String(release.Signature!)),
                        cancellationToken);
                    delivered++;
                    commandsSent++;
                }
                catch (Exception ex)
                {
                    logger.LogWarning(
                        ex,
                        "failed to send release {ReleaseId} to agent {AgentId} for deployment {DeploymentId}",
                        release.Id,
                        agent.Id,
                        deployment.Id);
                }
            }

            if (delivered == 0)
            {
                // Targets matched but nobody received the command: mark the wave as
                // skipped instead of leaving it running forever.
                wave.Status = DeploymentWaveStatus.Skipped;
                wave.CompletedAt = DateTimeOffset.UtcNow;
            }
        }

        if (commandsSent == 0)
        {
            deployment.Status = DeploymentStatus.Failed;
            deployment.CompletedAt = DateTimeOffset.UtcNow;
            foreach (var wave in deployment.Waves.Where(w => w.Status == DeploymentWaveStatus.Running))
            {
                wave.Status = DeploymentWaveStatus.Failed;
                wave.CompletedAt = deployment.CompletedAt;
            }
        }

        await deployments.UpdateAsync(deployment, cancellationToken);
        await AuditAsync(
            actor,
            deployment.Status == DeploymentStatus.Failed ? "deployment.failed" : "deployment.start",
            deployment.Id.ToString("D"),
            cancellationToken);
        return deployment;
    }

    /// <inheritdoc />
    public async Task RollbackAsync(
        string environment,
        Guid deploymentId,
        string actor,
        CancellationToken cancellationToken)
    {
        var deployment = await deployments.FindAsync(deploymentId, cancellationToken)
                         ?? throw new InvalidOperationException("Deployment not found.");

        if (!string.Equals(deployment.Environment, environment, StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException(
                $"Deployment belongs to environment '{deployment.Environment}', not '{environment}'.");
        }

        if (deployment.Status == DeploymentStatus.RolledBack)
        {
            throw new InvalidOperationException("Deployment was already rolled back.");
        }

        var releaseList = await releases.ListAsync(environment, cancellationToken);
        var currentIndex = releaseList.ToList().FindIndex(r => r.Id == deployment.ReleaseId);
        if (currentIndex < 0 || currentIndex + 1 >= releaseList.Count)
        {
            throw new InvalidOperationException("No previous release is available for rollback.");
        }

        var previous = releaseList[currentIndex + 1];

        // Verify before changing anything: a rollback to a release with a broken
        // signature must leave the current deployment untouched.
        await EnsureSignatureAsync(previous, cancellationToken);

        deployment.Status = DeploymentStatus.RolledBack;
        deployment.CompletedAt = DateTimeOffset.UtcNow;
        await deployments.UpdateAsync(deployment, cancellationToken);

        await DeployAsync(
            environment,
            previous.Id,
            deployment.Waves.Select(w => w.Selector).ToList(),
            actor,
            cancellationToken);

        await AuditAsync(actor, "deployment.rollback", deploymentId.ToString("D"), cancellationToken);
    }

    /// <inheritdoc />
    public async Task RevokeAsync(
        string environment,
        Guid releaseId,
        string actor,
        CancellationToken cancellationToken)
    {
        var release = await releases.FindAsync(releaseId, cancellationToken)
                      ?? throw new InvalidOperationException("Release not found.");

        if (!string.Equals(release.Environment, environment, StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException(
                $"Release belongs to environment '{release.Environment}', not '{environment}'.");
        }

        if (release.Status == ReleaseStatus.Revoked)
        {
            return;
        }

        await releases.SetStatusAsync(releaseId, ReleaseStatus.Revoked, cancellationToken);
        await AuditAsync(actor, "release.revoke", releaseId.ToString("D"), cancellationToken);
    }

    /// <summary>Apply timeout in milliseconds, bounded for the protobuf field.</summary>
    private int ApplyTimeoutMs() =>
        (int)Math.Clamp(options.ApplyTimeout.TotalMilliseconds, 1_000, int.MaxValue);

    /// <summary>Verifies the release manifest before anything is sent to agents.</summary>
    /// <param name="release">Release loaded from the database.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// The check catches an altered or partially written database row and a
    /// replaced signing key, not a compromised control-plane process: a process
    /// that can rewrite rows can also rewrite the signature.
    /// </remarks>
    /// <exception cref="InvalidOperationException">
    /// The release has no signature or the signature does not match.
    /// </exception>
    private async Task EnsureSignatureAsync(Release release, CancellationToken cancellationToken)
    {
        if (string.IsNullOrEmpty(release.Signature))
        {
            throw new InvalidOperationException("Release is not signed and cannot be deployed.");
        }

        if (!await signer.VerifyAsync(release, release.Signature, cancellationToken))
        {
            throw new InvalidOperationException(
                "Release signature does not match the stored manifest; the release was altered or the signing key changed.");
        }
    }

    /// <summary>Appends one audit event for a release-workflow operation.</summary>
    /// <param name="actor">Principal that performed the operation.</param>
    /// <param name="action">Stable action name.</param>
    /// <param name="target">Entity the action applies to.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private Task AuditAsync(string actor, string action, string target, CancellationToken cancellationToken) =>
        audit.AppendAsync(
            new AuditEvent
            {
                Actor = actor,
                Action = action,
                Target = target,
            },
            cancellationToken);
}
