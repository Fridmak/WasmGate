using System.Data;
using Dapper;
using Microsoft.Data.Sqlite;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the deployment store.
/// </summary>
/// <remarks>
/// A deployment spans <c>deployments</c> and <c>deployment_waves</c>; both are
/// written or updated in one immediate transaction. The wave column is named
/// <c>"index"</c> and must stay quoted in SQL.
/// </remarks>
public sealed class SqliteDeploymentRepository(SqliteConnectionFactory connections) : IDeploymentRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

    /// <inheritdoc />
    public async Task AddAsync(Deployment deployment, CancellationToken cancellationToken)
    {
        const string headerSql = """
            INSERT INTO deployments (id, environment, release_id, status, created_by,
                                     created_at, completed_at, message)
            VALUES (@Id, @Environment, @ReleaseId, @Status, @CreatedBy,
                    @CreatedAt, @CompletedAt, @Message)
            """;

        const string waveSql = """
            INSERT INTO deployment_waves (deployment_id, "index", selector, status,
                                          started_at, completed_at)
            VALUES (@DeploymentId, @Index, @Selector, @Status, @StartedAt, @CompletedAt)
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await using var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false);

        await connection.ExecuteAsync(headerSql, new
        {
            deployment.Id,
            deployment.Environment,
            deployment.ReleaseId,
            Status = deployment.Status.ToString(),
            deployment.CreatedBy,
            CreatedAt = SqliteTimestamps.ToParameter(deployment.CreatedAt),
            CompletedAt = SqliteTimestamps.ToParameter(deployment.CompletedAt),
            deployment.Message,
        }, transaction);

        foreach (var wave in deployment.Waves)
        {
            await connection.ExecuteAsync(waveSql, new
            {
                DeploymentId = deployment.Id,
                wave.Index,
                wave.Selector,
                Status = wave.Status.ToString(),
                StartedAt = SqliteTimestamps.ToParameter(wave.StartedAt),
                CompletedAt = SqliteTimestamps.ToParameter(wave.CompletedAt),
            }, transaction);
        }

        await transaction.CommitAsync(cancellationToken);
    }

    /// <inheritdoc />
    public async Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, release_id, status, created_by, created_at, completed_at, message
            FROM deployments
            WHERE id = @id
            """;

        const string waveSql = """
            SELECT deployment_id, "index", selector, status, started_at, completed_at
            FROM deployment_waves
            WHERE deployment_id = @id
            ORDER BY "index"
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var header = await connection.QuerySingleOrDefaultAsync<SqliteDeploymentRow>(headerSql, new { id });
        if (header is null)
        {
            return null;
        }

        var waves = await connection.QueryAsync<SqliteDeploymentWaveRow>(waveSql, new { id });
        return ToDeployment(header, waves.ToList());
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<Deployment>> ListActiveAsync(
        string environment,
        CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, release_id, status, created_by, created_at, completed_at, message
            FROM deployments
            WHERE environment = @environment AND status IN ('Pending', 'Running')
            ORDER BY created_at DESC, id DESC
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var headers = (await connection.QueryAsync<SqliteDeploymentRow>(headerSql, new { environment })).ToList();
        return await WithWavesAsync(connection, headers, cancellationToken);
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<Deployment>> ListAsync(
        string environment,
        int limit,
        CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, release_id, status, created_by, created_at, completed_at, message
            FROM deployments
            WHERE environment = @environment
            ORDER BY created_at DESC, id DESC
            LIMIT @limit
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var headers = (await connection.QueryAsync<SqliteDeploymentRow>(
            headerSql,
            new { environment, limit = Math.Clamp(limit, 1, 500) })).ToList();
        return await WithWavesAsync(connection, headers, cancellationToken);
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<Deployment>> ListStaleRunningAsync(
        DateTimeOffset createdBefore,
        CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, release_id, status, created_by, created_at, completed_at, message
            FROM deployments
            WHERE status = 'Running' AND created_at < @createdBefore
            ORDER BY created_at
            LIMIT 100
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var headers = (await connection.QueryAsync<SqliteDeploymentRow>(
            headerSql,
            new { createdBefore = SqliteTimestamps.ToParameter(createdBefore) })).ToList();
        return await WithWavesAsync(connection, headers, cancellationToken);
    }

    /// <inheritdoc />
    public async Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken)
    {
        const string headerSql = """
            UPDATE deployments
            SET status = @Status, completed_at = @CompletedAt, message = @Message
            WHERE id = @Id
            """;

        const string waveSql = """
            UPDATE deployment_waves
            SET status = @Status, started_at = @StartedAt, completed_at = @CompletedAt
            WHERE deployment_id = @DeploymentId AND "index" = @Index
            """;

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await using var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false);

        await connection.ExecuteAsync(headerSql, new
        {
            deployment.Id,
            Status = deployment.Status.ToString(),
            CompletedAt = SqliteTimestamps.ToParameter(deployment.CompletedAt),
            deployment.Message,
        }, transaction);

        foreach (var wave in deployment.Waves)
        {
            await connection.ExecuteAsync(waveSql, new
            {
                DeploymentId = deployment.Id,
                wave.Index,
                Status = wave.Status.ToString(),
                StartedAt = SqliteTimestamps.ToParameter(wave.StartedAt),
                CompletedAt = SqliteTimestamps.ToParameter(wave.CompletedAt),
            }, transaction);
        }

        await transaction.CommitAsync(cancellationToken);
    }

    private static async Task<IReadOnlyList<Deployment>> WithWavesAsync(
        SqliteConnection connection,
        List<SqliteDeploymentRow> headers,
        CancellationToken cancellationToken)
    {
        if (headers.Count == 0)
        {
            return [];
        }

        const string waveSql = """
            SELECT deployment_id, "index", selector, status, started_at, completed_at
            FROM deployment_waves
            WHERE deployment_id IN @ids
            ORDER BY deployment_id, "index"
            """;

        var ids = headers.Select(header => header.Id).ToArray();
        var waves = await connection.QueryAsync<SqliteDeploymentWaveRow>(waveSql, new { ids });

        var wavesByDeployment = waves
            .GroupBy(wave => wave.DeploymentId)
            .ToDictionary(group => group.Key, group => group.ToList());

        return headers
            .Select(header => ToDeployment(
                header,
                wavesByDeployment.GetValueOrDefault(header.Id) ?? []))
            .ToList();
    }

    private static Deployment ToDeployment(
        SqliteDeploymentRow header,
        IReadOnlyList<SqliteDeploymentWaveRow> waves) => new()
        {
            Id = header.Id,
            Environment = header.Environment,
            ReleaseId = header.ReleaseId,
            Status = header.Status,
            CreatedBy = header.CreatedBy,
            CreatedAt = SqliteTimestamps.FromColumn(header.CreatedAt),
            CompletedAt = SqliteTimestamps.FromColumn(header.CompletedAt),
            Message = header.Message,
            Waves = waves
                .Select(wave => new DeploymentWave
                {
                    Index = wave.Index,
                    Selector = wave.Selector,
                    Status = wave.Status,
                    StartedAt = SqliteTimestamps.FromColumn(wave.StartedAt),
                    CompletedAt = SqliteTimestamps.FromColumn(wave.CompletedAt),
                })
                .ToList(),
        };
}

/// <summary>Row shape of <c>deployments</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteDeploymentRow
{
    public Guid Id { get; set; }

    public string Environment { get; set; } = string.Empty;

    public Guid ReleaseId { get; set; }

    public DeploymentStatus Status { get; set; }

    public string CreatedBy { get; set; } = string.Empty;

    public long CreatedAt { get; set; }

    public long? CompletedAt { get; set; }

    public string? Message { get; set; }
}

/// <summary>Row shape of <c>deployment_waves</c> as read by Dapper.AOT.</summary>
internal sealed class SqliteDeploymentWaveRow
{
    public Guid DeploymentId { get; set; }

    public int Index { get; set; }

    public string Selector { get; set; } = "*";

    public DeploymentWaveStatus Status { get; set; }

    public long? StartedAt { get; set; }

    public long? CompletedAt { get; set; }
}
