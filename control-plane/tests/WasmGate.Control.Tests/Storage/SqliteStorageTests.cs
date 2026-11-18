using Microsoft.Data.Sqlite;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Core.Entities.Releases;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Tests.Storage;

/// <summary>
/// Integration tests for the SQLite repositories. Each test gets a fresh file,
/// so the suite exercises the real schema, Dapper.AOT mappings and WAL settings.
/// </summary>
public sealed class SqliteStorageTests
{
    [Fact]
    public async Task SchemaCreatesTablesAndSeedsEnvironments()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();

        await using var connection = await db.Connections.OpenAsync(CancellationToken.None);
        var tables = new List<string>();
        await using (var command = connection.CreateCommand())
        {
            command.CommandText = "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name";
            await using var reader = await command.ExecuteReaderAsync();
            while (await reader.ReadAsync())
            {
                tables.Add(reader.GetString(0));
            }
        }
        foreach (var expected in new[]
                 {
                     "agents", "agent_certificates", "artifacts", "audit_events", "deleted_agents",
                     "deployment_waves", "deployments", "enrollment_tokens", "environments",
                     "release_artifacts", "releases", "config_versions",
                 })
        {
            Assert.Contains(expected, tables);
        }

        await using var versionCommand = connection.CreateCommand();
        versionCommand.CommandText = "PRAGMA user_version";
        Assert.Equal(1L, Convert.ToInt64(await versionCommand.ExecuteScalarAsync()));

        var environments = await new SqliteEnvironmentRepository(db.Connections)
            .ListAsync(CancellationToken.None);
        Assert.Equal(["prod", "staging"], environments);
    }

    [Fact]
    public async Task ArtifactRepositoryAddsFindsAndLists()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteArtifactRepository(db.Connections);
        var createdAt = DateTimeOffset.UtcNow.AddMinutes(-1);
        var artifact = new Artifact
        {
            Id = new string('a', 64),
            Kind = ArtifactKind.Wasm,
            Size = 1024,
            MediaType = "application/wasm",
            CreatedBy = "tester",
            CreatedAt = createdAt,
        };

        await repository.AddAsync(artifact, CancellationToken.None);
        await repository.AddAsync(new Artifact
        {
            Id = artifact.Id,
            Kind = ArtifactKind.Other,
            Size = 1,
            CreatedBy = "second",
            CreatedAt = DateTimeOffset.UtcNow,
        }, CancellationToken.None);

        var found = await repository.FindAsync(artifact.Id, CancellationToken.None);
        Assert.NotNull(found);
        Assert.Equal(ArtifactKind.Wasm, found.Kind);
        Assert.Equal(1024, found.Size);
        Assert.Equal(createdAt.ToUnixTimeMilliseconds(), found.CreatedAt.ToUnixTimeMilliseconds());

        var list = await repository.ListAsync(CancellationToken.None);
        Assert.Single(list);
        Assert.Equal("tester", list[0].CreatedBy);
    }

    [Fact]
    public async Task AuditLogAppendsAndListsNewestFirst()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteAuditLog(db.Connections);

        await repository.AppendAsync(new AuditEvent
        {
            Actor = "one",
            Action = "agent.create",
            Target = "a",
            DetailsJson = "{\"x\":1}",
        }, CancellationToken.None);
        await repository.AppendAsync(new AuditEvent
        {
            Actor = "two",
            Action = "agent.delete",
            Target = "b",
        }, CancellationToken.None);

        var events = await repository.ListAsync(10, CancellationToken.None);
        Assert.Equal(2, events.Count);
        Assert.Equal("two", events[0].Actor);
        Assert.Equal("{\"x\":1}", events[1].DetailsJson);
        Assert.True(events[1].Id < events[0].Id);
        Assert.Empty(await repository.ListAsync(0, CancellationToken.None));
    }

    [Fact]
    public async Task EnvironmentRepositoryGuardsDeletion()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteEnvironmentRepository(db.Connections);
        var agents = new SqliteAgentRepository(db.Connections);

        Assert.True(await repository.EnsureAsync("lab", "tester", CancellationToken.None));
        Assert.False(await repository.EnsureAsync("lab", "tester", CancellationToken.None));
        Assert.Contains("lab", await repository.ListAsync(CancellationToken.None));

        await Assert.ThrowsAsync<InvalidOperationException>(
            () => repository.DeleteAsync("prod", CancellationToken.None));

        await agents.UpsertAsync(NewAgent("lab-agent", "lab"), CancellationToken.None);
        await Assert.ThrowsAsync<InvalidOperationException>(
            () => repository.DeleteAsync("lab", CancellationToken.None));

        Assert.True(await agents.DeleteAsync(
            (await agents.FindByNameAsync("lab-agent", CancellationToken.None))!.Id,
            CancellationToken.None));
        await repository.DeleteAsync("lab", CancellationToken.None);
        Assert.DoesNotContain("lab", await repository.ListAsync(CancellationToken.None));
    }

    [Fact]
    public async Task EnrollmentTokenConsumeIsAtomicAndBounded()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteEnrollmentTokenRepository(db.Connections);
        var token = new EnrollmentToken
        {
            TokenHash = new string('b', 64),
            Environment = "prod",
            MaxUses = 5,
            ExpiresAt = DateTimeOffset.UtcNow.AddHours(1),
        };
        await repository.AddAsync(token, CancellationToken.None);

        var results = await Task.WhenAll(Enumerable.Range(0, 20).Select(_ =>
            repository.ConsumeAsync(token.Id, CancellationToken.None)));

        Assert.Equal(5, results.Count(result => result));
        var stored = await repository.FindByHashAsync(token.TokenHash, CancellationToken.None);
        Assert.Equal(5, stored!.UsedCount);
    }

    [Fact]
    public async Task CertificateRepositoryTracksActiveAndRevoked()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var agents = new SqliteAgentRepository(db.Connections);
        var agent = NewAgent("cert-agent", "prod");
        await agents.UpsertAsync(agent, CancellationToken.None);

        var repository = new SqliteAgentCertificateRepository(db.Connections);
        var certificate = new AgentCertificate
        {
            AgentId = agent.Id,
            SerialNumber = "01",
            Thumbprint = new string('c', 64),
            Pem = "-----BEGIN CERTIFICATE-----",
            NotBefore = DateTimeOffset.UtcNow.AddMinutes(-1),
            NotAfter = DateTimeOffset.UtcNow.AddHours(1),
        };
        await repository.AddAsync(certificate, CancellationToken.None);

        Assert.Single(await repository.ListActiveByAgentAsync(agent.Id, CancellationToken.None));
        Assert.True(await repository.IsActiveAsync(agent.Id, certificate.Thumbprint, CancellationToken.None));

        await repository.RevokeAsync(agent.Id, certificate.SerialNumber, CancellationToken.None);
        Assert.Empty(await repository.ListActiveByAgentAsync(agent.Id, CancellationToken.None));
        Assert.False(await repository.IsActiveAsync(agent.Id, certificate.Thumbprint, CancellationToken.None));
    }

    [Fact]
    public async Task ConfigVersionHashIsUniquePerEnvironment()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteConfigVersionRepository(db.Connections);
        var first = NewConfig("prod", "same-hash");
        var duplicate = NewConfig("prod", "same-hash");
        var otherEnvironment = NewConfig("staging", "same-hash");

        await repository.AddAsync(first, CancellationToken.None);
        await repository.AddAsync(duplicate, CancellationToken.None);
        await repository.AddAsync(otherEnvironment, CancellationToken.None);

        Assert.NotNull(await repository.FindByHashAsync("prod", "same-hash", CancellationToken.None));
        Assert.NotNull(await repository.FindByHashAsync("staging", "same-hash", CancellationToken.None));
        Assert.Single(await repository.ListAsync("prod", CancellationToken.None));
    }

    [Fact]
    public async Task AgentRepositoryUpsertsTouchesDeletesAndBlocksResurrection()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteAgentRepository(db.Connections);
        var agent = new Agent
        {
            Name = "dp-01",
            Environment = "prod",
            Version = "dev",
            AbiVersions = ["4"],
            Labels = new Dictionary<string, string> { ["role"] = "demo" },
            RegisteredAt = DateTimeOffset.UtcNow,
            LastSeenAt = DateTimeOffset.UtcNow,
        };

        Assert.True(await repository.UpsertAsync(agent, CancellationToken.None));
        var found = await repository.FindByNameAsync("dp-01", CancellationToken.None);
        Assert.NotNull(found);
        Assert.Equal(agent.Id, found.Id);
        Assert.Equal(["4"], found.AbiVersions);
        Assert.Equal("demo", found.Labels["role"]);
        Assert.Equal(AgentStatus.Pending, found.Status);

        agent.Status = AgentStatus.Online;
        agent.LastSeenAt = DateTimeOffset.UtcNow.AddSeconds(1);
        agent.ActiveReleaseId = "release-1";
        Assert.True(await repository.UpsertAsync(agent, CancellationToken.None));
        found = await repository.FindAsync(agent.Id, CancellationToken.None);
        Assert.Equal(AgentStatus.Online, found!.Status);
        Assert.Equal("release-1", found.ActiveReleaseId);

        await repository.TouchAsync(
            agent.Id,
            DateTimeOffset.UtcNow.AddSeconds(2),
            null,
            "",
            CancellationToken.None);
        found = await repository.FindAsync(agent.Id, CancellationToken.None);
        Assert.Equal("release-1", found!.ActiveReleaseId);
        Assert.Null(found.ActiveConfigSha256);

        Assert.True(await repository.DeleteAsync(agent.Id, CancellationToken.None));
        Assert.False(await repository.DeleteAsync(agent.Id, CancellationToken.None));
        Assert.False(await repository.UpsertAsync(agent, CancellationToken.None));
        Assert.Null(await repository.FindAsync(agent.Id, CancellationToken.None));
    }

    [Fact]
    public async Task ReleaseRepositoryWritesHeaderAndArtifactsAtomically()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var artifacts = new SqliteArtifactRepository(db.Connections);
        var configs = new SqliteConfigVersionRepository(db.Connections);
        var releases = new SqliteReleaseRepository(db.Connections);

        var artifact = new Artifact
        {
            Id = new string('d', 64),
            Kind = ArtifactKind.Wasm,
            Size = 10,
            CreatedAt = DateTimeOffset.UtcNow,
        };
        await artifacts.AddAsync(artifact, CancellationToken.None);
        var config = NewConfig("prod", "cfg-hash");
        await configs.AddAsync(config, CancellationToken.None);

        var release = new Release
        {
            Environment = "prod",
            ConfigVersionId = config.Id,
            ConfigSha256 = config.Sha256,
            CreatedBy = "tester",
            Status = ReleaseStatus.Validated,
            Artifacts = [new ReleaseArtifact { ArtifactId = artifact.Id, Name = "plugins/a.wasm" }],
        };
        await releases.AddAsync(release, CancellationToken.None);

        var found = await releases.FindAsync(release.Id, CancellationToken.None);
        Assert.NotNull(found);
        Assert.Single(found.Artifacts);
        Assert.Equal("plugins/a.wasm", found.Artifacts[0].Name);
        Assert.Equal(ReleaseStatus.Validated, found.Status);

        await releases.SetStatusAsync(release.Id, ReleaseStatus.Revoked, CancellationToken.None);
        Assert.Equal(ReleaseStatus.Revoked,
            (await releases.FindAsync(release.Id, CancellationToken.None))!.Status);
        Assert.Single(await releases.ListAsync("prod", CancellationToken.None));
    }

    [Fact]
    public async Task DeploymentRepositoryWritesWavesAndFiltersStates()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var artifacts = new SqliteArtifactRepository(db.Connections);
        var configs = new SqliteConfigVersionRepository(db.Connections);
        var releases = new SqliteReleaseRepository(db.Connections);
        var deployments = new SqliteDeploymentRepository(db.Connections);

        var config = NewConfig("prod", "deploy-hash");
        await configs.AddAsync(config, CancellationToken.None);
        var artifact = new Artifact
        {
            Id = new string('e', 64),
            Kind = ArtifactKind.Wasm,
            Size = 1,
            CreatedAt = DateTimeOffset.UtcNow,
        };
        await artifacts.AddAsync(artifact, CancellationToken.None);
        var release = new Release
        {
            Environment = "prod",
            ConfigVersionId = config.Id,
            ConfigSha256 = config.Sha256,
            CreatedBy = "tester",
            Status = ReleaseStatus.Validated,
            Artifacts = [new ReleaseArtifact { ArtifactId = artifact.Id, Name = "a.wasm" }],
        };
        await releases.AddAsync(release, CancellationToken.None);

        var createdAt = DateTimeOffset.UtcNow.AddMinutes(-10);
        var deployment = new Deployment
        {
            Environment = "prod",
            ReleaseId = release.Id,
            CreatedBy = "tester",
            Status = DeploymentStatus.Running,
            CreatedAt = createdAt,
            Waves =
            [
                new DeploymentWave { Index = 0, Selector = "*", Status = DeploymentWaveStatus.Running },
                new DeploymentWave { Index = 1, Selector = "{\"role\":\"demo\"}" },
            ],
        };
        await deployments.AddAsync(deployment, CancellationToken.None);

        var found = await deployments.FindAsync(deployment.Id, CancellationToken.None);
        Assert.NotNull(found);
        Assert.Equal(2, found.Waves.Count);
        Assert.Equal("*", found.Waves[0].Selector);
        Assert.Single(await deployments.ListActiveAsync("prod", CancellationToken.None));
        Assert.Single(await deployments.ListStaleRunningAsync(
            DateTimeOffset.UtcNow.AddMinutes(-5), CancellationToken.None));

        deployment.Status = DeploymentStatus.Succeeded;
        deployment.CompletedAt = DateTimeOffset.UtcNow;
        deployment.Message = "ok";
        deployment.Waves[0].Status = DeploymentWaveStatus.Succeeded;
        deployment.Waves[0].CompletedAt = DateTimeOffset.UtcNow;
        await deployments.UpdateAsync(deployment, CancellationToken.None);

        found = await deployments.FindAsync(deployment.Id, CancellationToken.None);
        Assert.Equal(DeploymentStatus.Succeeded, found!.Status);
        Assert.Equal("ok", found.Message);
        Assert.Equal(DeploymentWaveStatus.Succeeded, found.Waves[0].Status);
        Assert.Empty(await deployments.ListActiveAsync("prod", CancellationToken.None));
        Assert.Single(await deployments.ListAsync("prod", 10, CancellationToken.None));
    }

    [Fact]
    public async Task ConcurrentAuditWritersDoNotLoseRows()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var audit = new SqliteAuditLog(db.Connections);

        await Task.WhenAll(Enumerable.Range(0, 4).Select(async worker =>
        {
            for (var i = 0; i < 50; i++)
            {
                await audit.AppendAsync(new AuditEvent
                {
                    Actor = $"worker-{worker}",
                    Action = "test",
                    Target = i.ToString(),
                }, CancellationToken.None);
            }
        }));

        Assert.Equal(200, (await audit.ListAsync(1000, CancellationToken.None)).Count);
    }

    [Fact]
    public async Task EnvironmentLockSerializesTheSameEnvironment()
    {
        var gate = new SqliteEnvironmentOperationLock();
        var order = new List<string>();

        var first = await gate.AcquireAsync("prod", CancellationToken.None);
        order.Add("first-enter");
        var secondTask = Task.Run(async () =>
        {
            await using var second = await gate.AcquireAsync("prod", CancellationToken.None);
            order.Add("second-enter");
        });
        await Task.Delay(100);
        await first.DisposeAsync();
        order.Add("first-leave");
        await secondTask;

        Assert.Equal(["first-enter", "first-leave", "second-enter"], order);
    }

    [Fact]
    public async Task SchemaEnsureIsIdempotentAndRejectsNewerVersions()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        await using (var connection = await db.Connections.OpenAsync(CancellationToken.None))
        {
            await SqliteSchema.EnsureAsync(connection, CancellationToken.None);
            await using var newer = connection.CreateCommand();
            newer.CommandText = "PRAGMA user_version = 99";
            await newer.ExecuteNonQueryAsync();
            await Assert.ThrowsAsync<InvalidOperationException>(
                () => SqliteSchema.EnsureAsync(connection, CancellationToken.None));
        }
    }

    [Fact]
    public async Task ParallelEnvironmentEnsureCreatesExactlyOneRow()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteEnvironmentRepository(db.Connections);

        var results = await Task.WhenAll(Enumerable.Range(0, 10).Select(_ =>
            repository.EnsureAsync("race", "tester", CancellationToken.None)));

        Assert.Equal(1, results.Count(result => result));
        Assert.Single(await repository.ListAsync(CancellationToken.None), name => name == "race");
    }

    [Fact]
    public async Task UpsertAndDeleteRaceNeverResurrectsAnAgent()
    {
        await using var db = await SqliteTestDatabase.CreateAsync();
        var repository = new SqliteAgentRepository(db.Connections);

        for (var i = 0; i < 20; i++)
        {
            var agent = NewAgent($"race-{i}", "prod");
            await repository.UpsertAsync(agent, CancellationToken.None);

            var upsert = repository.UpsertAsync(agent, CancellationToken.None);
            var delete = repository.DeleteAsync(agent.Id, CancellationToken.None);
            await Task.WhenAll(upsert, delete);

            Assert.Null(await repository.FindAsync(agent.Id, CancellationToken.None));
            Assert.False(await repository.UpsertAsync(agent, CancellationToken.None));
        }
    }

    [Fact]
    public void TimestampsRoundTripThroughUnixMilliseconds()
    {
        var value = DateTimeOffset.UtcNow;
        var stored = SqliteTimestamps.ToParameter(value);
        var restored = SqliteTimestamps.FromColumn(stored);

        Assert.Equal(value.ToUnixTimeMilliseconds(), restored.ToUnixTimeMilliseconds());
        Assert.Equal(TimeSpan.Zero, restored.Offset);
    }

    private static Agent NewAgent(string name, string environment) => new()
    {
        Name = name,
        Environment = environment,
        Version = "dev",
        RegisteredAt = DateTimeOffset.UtcNow,
        LastSeenAt = DateTimeOffset.UtcNow,
    };

    private static ConfigVersion NewConfig(string environment, string sha256) => new()
    {
        Environment = environment,
        Sha256 = sha256,
        Yaml = "version: 1",
        CreatedBy = "tester",
        CreatedAt = DateTimeOffset.UtcNow,
    };
}

/// <summary>Fresh SQLite database for one test.</summary>
internal sealed class SqliteTestDatabase : IAsyncDisposable
{
    private readonly string _directory;

    private SqliteTestDatabase(
        string directory,
        string databasePath,
        SqliteConnectionFactory connections)
    {
        _directory = directory;
        DatabasePath = databasePath;
        Connections = connections;
    }

    public string DatabasePath { get; }

    public SqliteConnectionFactory Connections { get; }

    public static async Task<SqliteTestDatabase> CreateAsync()
    {
        var directory = Path.Combine(
            Path.GetTempPath(),
            "wasmgate-cp-tests",
            Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(directory);
        var databasePath = Path.Combine(directory, "control.db");
        var connectionString = new SqliteConnectionStringBuilder
        {
            DataSource = databasePath,
            Mode = SqliteOpenMode.ReadWriteCreate,
            Pooling = true,
            ForeignKeys = true,
            DefaultTimeout = 5,
        }.ToString();
        var database = new SqliteTestDatabase(
            directory,
            databasePath,
            new SqliteConnectionFactory(connectionString));

        await using var connection = await database.Connections.OpenAsync(CancellationToken.None);
        await SqliteSchema.EnsureAsync(connection, CancellationToken.None);
        return database;
    }

    public ValueTask DisposeAsync()
    {
        SqliteConnection.ClearAllPools();
        try
        {
            Directory.Delete(_directory, recursive: true);
        }
        catch (IOException)
        {
            // A leaked pool handle on Windows only affects temp cleanup.
        }

        return ValueTask.CompletedTask;
    }
}
