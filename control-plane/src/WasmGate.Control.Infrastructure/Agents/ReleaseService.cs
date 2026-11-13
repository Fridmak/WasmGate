using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Minimal release workflow: immutable configs, validated and signed releases,
/// wave-based deployment and rollback. Deployment progress is updated by the
/// agent apply results (not yet wired to the gRPC session) and by the operator.
/// </summary>
public sealed class ReleaseService(
    IConfigVersionRepository configs,
    IArtifactRepository artifactRepository,
    IReleaseRepository releases,
    IDeploymentRepository deployments,
    IAgentRepository agents,
    IAgentSessionRegistry sessions,
    IConfigValidator validator,
    IReleaseSigner signer,
    IAuditLog audit) : IReleaseService
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

        var sha = Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(yaml))).ToLowerInvariant();
        var existing = await configs.FindByHashAsync(sha, cancellationToken);
        if (existing is not null)
        {
            return existing;
        }

        var version = new ConfigVersion
        {
            Environment = environment,
            Sha256 = sha,
            Yaml = yaml,
            Message = message,
            CreatedBy = actor,
        };
        await configs.AddAsync(version, cancellationToken);
        await audit.AppendAsync(
            new AuditEvent
            {
                Actor = actor,
                Action = "config.create",
                Target = version.Id.ToString("D"),
                DetailsJson = null,
            },
            cancellationToken);
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
        var config = await configs.FindAsync(configVersionId, cancellationToken)
                     ?? throw new InvalidOperationException("Configuration version not found.");

        foreach (var artifact in artifacts)
        {
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
        await audit.AppendAsync(
            new AuditEvent
            {
                Actor = actor,
                Action = "release.create",
                Target = release.Id.ToString("D"),
                DetailsJson = null,
            },
            cancellationToken);
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
        var release = await releases.FindAsync(releaseId, cancellationToken)
                      ?? throw new InvalidOperationException("Release not found.");
        if (release.Status == ReleaseStatus.Revoked)
        {
            throw new InvalidOperationException("Release is revoked.");
        }

        var config = await configs.FindAsync(release.ConfigVersionId, cancellationToken)
                     ?? throw new InvalidOperationException("Configuration version not found.");

        var deployment = new Deployment
        {
            Environment = environment,
            ReleaseId = releaseId,
            CreatedBy = actor,
            Status = DeploymentStatus.Running,
            Waves = waveSelectors.Count == 0
                ? [new DeploymentWave { Index = 0, Selector = "*" }]
                : waveSelectors
                    .Select((selector, index) => new DeploymentWave { Index = index, Selector = selector })
                    .ToList(),
        };
        await deployments.AddAsync(deployment, cancellationToken);

        var allAgents = await agents.ListAsync(cancellationToken);
        var artifactRefs = new List<AgentArtifact>();
        foreach (var reference in release.Artifacts)
        {
            var artifact = await artifactRepository.FindAsync(reference.ArtifactId, cancellationToken);
            if (artifact is not null)
            {
                artifactRefs.Add(new AgentArtifact(
                    artifact.Id,
                    reference.Name,
                    artifact.Kind,
                    artifact.Size,
                    artifact.Signature));
            }
        }

        foreach (var wave in deployment.Waves)
        {
            var targets = allAgents.Where(agent => Matches(agent, wave.Selector)).ToList();
            wave.Status = targets.Count == 0 ? DeploymentWaveStatus.Skipped : DeploymentWaveStatus.Running;
            wave.StartedAt = DateTimeOffset.UtcNow;

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
                            TimeoutMs: 30000,
                            ConfigSha256: config.Sha256,
                            ConfigYaml: config.Yaml,
                            Artifacts: artifactRefs,
                            DeploymentId: deployment.Id.ToString("D")),
                        cancellationToken);
                }
                catch (Exception)
                {
                    // A failed send is left for the heartbeat reaper / next deployment.
                }
            }
        }

        await deployments.UpdateAsync(deployment, cancellationToken);
        await audit.AppendAsync(
            new AuditEvent
            {
                Actor = actor,
                Action = "deployment.start",
                Target = deployment.Id.ToString("D"),
                DetailsJson = null,
            },
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

        var releaseList = await releases.ListAsync(environment, cancellationToken);
        var currentIndex = releaseList.ToList().FindIndex(r => r.Id == deployment.ReleaseId);
        if (currentIndex < 0 || currentIndex + 1 >= releaseList.Count)
        {
            throw new InvalidOperationException("No previous release is available for rollback.");
        }

        var previous = releaseList[currentIndex + 1];
        deployment.Status = DeploymentStatus.RolledBack;
        deployment.CompletedAt = DateTimeOffset.UtcNow;
        await deployments.UpdateAsync(deployment, cancellationToken);

        await DeployAsync(
            environment,
            previous.Id,
            deployment.Waves.Select(w => w.Selector).ToList(),
            actor,
            cancellationToken);

        await audit.AppendAsync(
            new AuditEvent
            {
                Actor = actor,
                Action = "deployment.rollback",
                Target = deploymentId.ToString("D"),
                DetailsJson = null,
            },
            cancellationToken);
    }

    private static bool Matches(Agent agent, string selector)
    {
        if (string.IsNullOrWhiteSpace(selector) || selector == "*")
        {
            return true;
        }

        try
        {
            using var document = JsonDocument.Parse(selector);
            if (document.RootElement.ValueKind != JsonValueKind.Object)
            {
                return false;
            }

            foreach (var property in document.RootElement.EnumerateObject())
            {
                if (!agent.Labels.TryGetValue(property.Name, out var value) ||
                    !string.Equals(value, property.Value.GetString(), StringComparison.Ordinal))
                {
                    return false;
                }
            }

            return true;
        }
        catch (JsonException)
        {
            return false;
        }
    }
}

