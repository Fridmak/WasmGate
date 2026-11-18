using Npgsql;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Tests.Storage;

/// <summary>
/// PostgreSQL smoke tests. They run only when <c>WASMGATE_TEST_POSTGRES</c> is
/// set, so a normal local <c>dotnet test</c> does not require a database.
/// </summary>
public sealed class PostgresStorageTests
{
    [Fact]
    public async Task AgentAuditArtifactAndProbeWork()
    {
        var connectionString = Environment.GetEnvironmentVariable("WASMGATE_TEST_POSTGRES");
        if (string.IsNullOrWhiteSpace(connectionString))
        {
            return;
        }

        await using var dataSource = new NpgsqlDataSourceBuilder(connectionString).Build();
        var suffix = Guid.NewGuid().ToString("N");
        var agents = new PostgresAgentRepository(dataSource);
        var audit = new PostgresAuditLog(dataSource);
        var artifacts = new PostgresArtifactRepository(dataSource);
        var environments = new PostgresEnvironmentRepository(dataSource);
        var probe = new PostgresStorageProbe(dataSource);

        Assert.True(await environments.EnsureAsync($"ci-{suffix}", "tests", CancellationToken.None));

        var agent = new Agent
        {
            Name = $"ci-agent-{suffix}",
            Environment = "prod",
            Version = "test",
            AbiVersions = ["4"],
            Labels = new Dictionary<string, string> { ["role"] = "ci" },
        };
        Assert.True(await agents.UpsertAsync(agent, CancellationToken.None));
        var found = await agents.FindAsync(agent.Id, CancellationToken.None);
        Assert.NotNull(found);
        Assert.Equal(["4"], found.AbiVersions);
        Assert.Equal("ci", found.Labels["role"]);

        await audit.AppendAsync(new AuditEvent
        {
            Actor = "tests",
            Action = "agent.create",
            Target = agent.Id.ToString("D"),
        }, CancellationToken.None);
        Assert.Contains(
            await audit.ListAsync(10, CancellationToken.None),
            item => item.Target == agent.Id.ToString("D"));

        var artifact = new Artifact
        {
            Id = Convert.ToHexString(Guid.NewGuid().ToByteArray()).ToLowerInvariant()
                + Convert.ToHexString(Guid.NewGuid().ToByteArray()).ToLowerInvariant(),
            Kind = ArtifactKind.Wasm,
            Size = 1,
            CreatedAt = DateTimeOffset.UtcNow,
        };
        await artifacts.AddAsync(artifact, CancellationToken.None);
        Assert.NotNull(await artifacts.FindAsync(artifact.Id, CancellationToken.None));

        Assert.True(await probe.CheckAsync(CancellationToken.None));
        Assert.True(await agents.DeleteAsync(agent.Id, CancellationToken.None));
        await environments.DeleteAsync($"ci-{suffix}", CancellationToken.None);
    }

    [Fact]
    public async Task ReleaseDeploymentConfigCertificateAndEnrollmentWork()
    {
        var connectionString = Environment.GetEnvironmentVariable("WASMGATE_TEST_POSTGRES");
        if (string.IsNullOrWhiteSpace(connectionString))
        {
            return;
        }

        await using var dataSource = new NpgsqlDataSourceBuilder(connectionString).Build();
        var suffix = Guid.NewGuid().ToString("N");
        var environment = $"ci2-{suffix}";
        var environments = new PostgresEnvironmentRepository(dataSource);
        var configs = new PostgresConfigVersionRepository(dataSource);
        var artifacts = new PostgresArtifactRepository(dataSource);
        var releases = new PostgresReleaseRepository(dataSource);
        var deployments = new PostgresDeploymentRepository(dataSource);
        var tokens = new PostgresEnrollmentTokenRepository(dataSource);
        var agents = new PostgresAgentRepository(dataSource);
        var certificates = new PostgresAgentCertificateRepository(dataSource);

        Assert.True(await environments.EnsureAsync(environment, "tests", CancellationToken.None));

        var config = new ConfigVersion
        {
            Environment = environment,
            Sha256 = $"{suffix}-config",
            Yaml = "version: 1",
            CreatedBy = "tests",
        };
        await configs.AddAsync(config, CancellationToken.None);
        await configs.AddAsync(config, CancellationToken.None);
        Assert.NotNull(await configs.FindByHashAsync(environment, config.Sha256, CancellationToken.None));
        Assert.Single(await configs.ListAsync(environment, CancellationToken.None));

        var artifact = new Artifact
        {
            Id = Convert.ToHexString(Guid.NewGuid().ToByteArray()).ToLowerInvariant()
                + Convert.ToHexString(Guid.NewGuid().ToByteArray()).ToLowerInvariant(),
            Kind = ArtifactKind.Wasm,
            Size = 1,
            CreatedAt = DateTimeOffset.UtcNow,
        };
        await artifacts.AddAsync(artifact, CancellationToken.None);

        var release = new Release
        {
            Environment = environment,
            ConfigVersionId = config.Id,
            ConfigSha256 = config.Sha256,
            CreatedBy = "tests",
            Status = ReleaseStatus.Validated,
            Artifacts = [new ReleaseArtifact { ArtifactId = artifact.Id, Name = "plugins/a.wasm" }],
        };
        await releases.AddAsync(release, CancellationToken.None);
        Assert.Single((await releases.FindAsync(release.Id, CancellationToken.None))!.Artifacts);
        await releases.SetStatusAsync(release.Id, ReleaseStatus.Revoked, CancellationToken.None);
        Assert.Equal(ReleaseStatus.Revoked,
            (await releases.FindAsync(release.Id, CancellationToken.None))!.Status);

        var deployment = new Deployment
        {
            Environment = environment,
            ReleaseId = release.Id,
            CreatedBy = "tests",
            Status = DeploymentStatus.Running,
            CreatedAt = DateTimeOffset.UtcNow.AddMinutes(-1),
            Waves =
            [
                new DeploymentWave { Index = 0, Selector = "*", Status = DeploymentWaveStatus.Running },
                new DeploymentWave { Index = 1, Selector = "{\"role\":\"ci\"}" },
            ],
        };
        await deployments.AddAsync(deployment, CancellationToken.None);
        Assert.Equal(2, (await deployments.FindAsync(deployment.Id, CancellationToken.None))!.Waves.Count);
        Assert.Single(await deployments.ListActiveAsync(environment, CancellationToken.None));
        Assert.Single(await deployments.ListStaleRunningAsync(
            DateTimeOffset.UtcNow.AddMinutes(1), CancellationToken.None));

        deployment.Status = DeploymentStatus.Succeeded;
        deployment.CompletedAt = DateTimeOffset.UtcNow;
        deployment.Message = "ok";
        deployment.Waves[0].Status = DeploymentWaveStatus.Succeeded;
        await deployments.UpdateAsync(deployment, CancellationToken.None);
        Assert.Empty(await deployments.ListActiveAsync(environment, CancellationToken.None));

        var token = new EnrollmentToken
        {
            TokenHash = Convert.ToHexString(Guid.NewGuid().ToByteArray()).ToLowerInvariant()
                + Convert.ToHexString(Guid.NewGuid().ToByteArray()).ToLowerInvariant(),
            Environment = environment,
            MaxUses = 2,
            ExpiresAt = DateTimeOffset.UtcNow.AddHours(1),
        };
        await tokens.AddAsync(token, CancellationToken.None);
        Assert.True(await tokens.ConsumeAsync(token.Id, CancellationToken.None));
        Assert.True(await tokens.ConsumeAsync(token.Id, CancellationToken.None));
        Assert.False(await tokens.ConsumeAsync(token.Id, CancellationToken.None));
        Assert.Equal(2, (await tokens.FindByHashAsync(token.TokenHash, CancellationToken.None))!.UsedCount);

        var agent = new Agent
        {
            Name = $"ci2-agent-{suffix}",
            Environment = environment,
            Version = "test",
        };
        await agents.UpsertAsync(agent, CancellationToken.None);
        var certificate = new AgentCertificate
        {
            AgentId = agent.Id,
            SerialNumber = suffix,
            Thumbprint = suffix,
            Pem = "pem",
            NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
            NotAfter = DateTimeOffset.UtcNow.AddHours(1),
        };
        await certificates.AddAsync(certificate, CancellationToken.None);
        Assert.Single(await certificates.ListActiveByAgentAsync(agent.Id, CancellationToken.None));
        Assert.True(await certificates.IsActiveAsync(agent.Id, certificate.Thumbprint, CancellationToken.None));
        await certificates.RevokeAsync(agent.Id, certificate.SerialNumber, CancellationToken.None);
        Assert.False(await certificates.IsActiveAsync(agent.Id, certificate.Thumbprint, CancellationToken.None));

        await Assert.ThrowsAsync<InvalidOperationException>(
            () => environments.DeleteAsync(environment, CancellationToken.None));
        Assert.True(await agents.DeleteAsync(agent.Id, CancellationToken.None));
        await environments.DeleteAsync(environment, CancellationToken.None);
    }
}
