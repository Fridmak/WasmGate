using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>
/// PostgreSQL implementation of the deployment store.
/// </summary>
/// <remarks>
/// <para>
/// A deployment spans two tables: <c>deployments</c> for the header and
/// <c>deployment_waves</c> for the waves. The wave column is named
/// <c>"index"</c> and must stay quoted in SQL.
/// </para>
/// <para>
/// <c>selector</c> is plain text, because it stores either the bare token
/// <c>*</c> or a JSON object.
/// </para>
/// </remarks>
public sealed class PostgresDeploymentRepository(NpgsqlDataSource dataSource) : IDeploymentRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task AddAsync(Deployment deployment, CancellationToken cancellationToken)
    {
        const string headerSql = """
            INSERT INTO deployments (id, environment, release_id, status, created_by,
                                     created_at, completed_at)
            VALUES (@Id, @Environment, @ReleaseId, @Status, @CreatedBy,
                    @CreatedAt, @CompletedAt)
            """;

        const string waveSql = """
            INSERT INTO deployment_waves (deployment_id, "index", selector, status,
                                          started_at, completed_at)
            VALUES (@DeploymentId, @Index, @Selector, @Status, @StartedAt, @CompletedAt)
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await using var transaction = await connection.BeginTransactionAsync(cancellationToken);

        await connection.ExecuteAsync(headerSql, new
        {
            deployment.Id,
            deployment.Environment,
            deployment.ReleaseId,
            Status = deployment.Status.ToString(),
            deployment.CreatedBy,
            CreatedAt = PostgresTimestamps.ToParameter(deployment.CreatedAt),
            CompletedAt = PostgresTimestamps.ToParameter(deployment.CompletedAt),
        }, transaction: transaction);

        foreach (var wave in deployment.Waves)
        {
            await connection.ExecuteAsync(waveSql, new
            {
                DeploymentId = deployment.Id,
                wave.Index,
                wave.Selector,
                Status = wave.Status.ToString(),
                StartedAt = PostgresTimestamps.ToParameter(wave.StartedAt),
                CompletedAt = PostgresTimestamps.ToParameter(wave.CompletedAt),
            }, transaction: transaction);
        }

        await transaction.CommitAsync(cancellationToken);
    }

    /// <inheritdoc />
    public async Task<Deployment?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, release_id, status, created_by, created_at, completed_at
            FROM deployments
            WHERE id = @id
            """;

        const string waveSql = """
            SELECT deployment_id, "index", selector, status, started_at, completed_at
            FROM deployment_waves
            WHERE deployment_id = @id
            ORDER BY "index"
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var header = await connection.QuerySingleOrDefaultAsync<DeploymentRow>(headerSql, new { id });
        if (header is null)
        {
            return null;
        }

        var waves = await connection.QueryAsync<DeploymentWaveRow>(waveSql, new { id });
        return ToDeployment(header, waves.ToList());
    }

    /// <inheritdoc />
    public async Task<IReadOnlyList<Deployment>> ListActiveAsync(
        string environment,
        CancellationToken cancellationToken)
    {
        const string headerSql = """
            SELECT id, environment, release_id, status, created_by, created_at, completed_at
            FROM deployments
            WHERE environment = @environment AND status IN ('Pending', 'Running')
            ORDER BY created_at DESC, id DESC
            """;

        const string waveSql = """
            SELECT deployment_id, "index", selector, status, started_at, completed_at
            FROM deployment_waves
            WHERE deployment_id = ANY(@ids)
            ORDER BY deployment_id, "index"
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var headers = (await connection.QueryAsync<DeploymentRow>(headerSql, new { environment })).ToList();
        if (headers.Count == 0)
        {
            return [];
        }

        var ids = headers.Select(header => header.Id).ToArray();
        var waves = await connection.QueryAsync<DeploymentWaveRow>(waveSql, new { ids });

        var wavesByDeployment = waves
            .GroupBy(wave => wave.DeploymentId)
            .ToDictionary(group => group.Key, group => group.ToList());

        return headers
            .Select(header => ToDeployment(
                header,
                wavesByDeployment.GetValueOrDefault(header.Id) ?? []))
            .ToList();
    }

    /// <inheritdoc />
    public async Task UpdateAsync(Deployment deployment, CancellationToken cancellationToken)
    {
        const string headerSql = """
            UPDATE deployments
            SET status = @Status, completed_at = @CompletedAt
            WHERE id = @Id
            """;

        const string waveSql = """
            UPDATE deployment_waves
            SET status = @Status, started_at = @StartedAt, completed_at = @CompletedAt
            WHERE deployment_id = @DeploymentId AND "index" = @Index
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await using var transaction = await connection.BeginTransactionAsync(cancellationToken);

        await connection.ExecuteAsync(headerSql, new
        {
            deployment.Id,
            Status = deployment.Status.ToString(),
            CompletedAt = PostgresTimestamps.ToParameter(deployment.CompletedAt),
        }, transaction: transaction);

        foreach (var wave in deployment.Waves)
        {
            await connection.ExecuteAsync(waveSql, new
            {
                DeploymentId = deployment.Id,
                wave.Index,
                Status = wave.Status.ToString(),
                StartedAt = PostgresTimestamps.ToParameter(wave.StartedAt),
                CompletedAt = PostgresTimestamps.ToParameter(wave.CompletedAt),
            }, transaction: transaction);
        }

        await transaction.CommitAsync(cancellationToken);
    }

    /// <summary>Combines a deployment header with its wave rows.</summary>
    /// <param name="header">Row read from <c>deployments</c>.</param>
    /// <param name="waves">Rows read from <c>deployment_waves</c>, ordered by index.</param>
    /// <returns>The mapped deployment.</returns>
    private static Deployment ToDeployment(DeploymentRow header, IReadOnlyList<DeploymentWaveRow> waves) => new()
    {
        Id = header.Id,
        Environment = header.Environment,
        ReleaseId = header.ReleaseId,
        Status = header.Status,
        CreatedBy = header.CreatedBy,
        CreatedAt = PostgresTimestamps.FromColumn(header.CreatedAt),
        CompletedAt = PostgresTimestamps.FromColumn(header.CompletedAt),
        Waves = waves
            .Select(wave => new DeploymentWave
            {
                Index = wave.Index,
                Selector = wave.Selector,
                Status = wave.Status,
                StartedAt = PostgresTimestamps.FromColumn(wave.StartedAt),
                CompletedAt = PostgresTimestamps.FromColumn(wave.CompletedAt),
            })
            .ToList(),
    };
}

/// <summary>Row shape of <c>deployments</c> as read by Dapper.AOT.</summary>
internal sealed class DeploymentRow
{
    /// <summary>Deployment identifier.</summary>
    public Guid Id { get; set; }

    /// <summary>Logical environment.</summary>
    public string Environment { get; set; } = string.Empty;

    /// <summary>Release being deployed.</summary>
    public Guid ReleaseId { get; set; }

    /// <summary>Lifecycle state.</summary>
    public DeploymentStatus Status { get; set; }

    /// <summary>Principal that started the deployment.</summary>
    public string CreatedBy { get; set; } = string.Empty;

    /// <summary>Creation time, in UTC.</summary>
    public DateTime CreatedAt { get; set; }

    /// <summary>Time the deployment reached a terminal state, in UTC, or null.</summary>
    public DateTime? CompletedAt { get; set; }
}

/// <summary>Row shape of <c>deployment_waves</c> as read by Dapper.AOT.</summary>
internal sealed class DeploymentWaveRow
{
    /// <summary>Owning deployment.</summary>
    public Guid DeploymentId { get; set; }

    /// <summary>0-based wave index.</summary>
    public int Index { get; set; }

    /// <summary>Either <c>*</c> or a JSON label selector.</summary>
    public string Selector { get; set; } = "*";

    /// <summary>Lifecycle state of the wave.</summary>
    public DeploymentWaveStatus Status { get; set; }

    /// <summary>Time the wave started, in UTC, or null.</summary>
    public DateTime? StartedAt { get; set; }

    /// <summary>Time the wave finished, in UTC, or null.</summary>
    public DateTime? CompletedAt { get; set; }
}
