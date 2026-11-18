using System.Data;
using System.Globalization;
using Dapper;
using Microsoft.Data.Sqlite;

[module: DapperAot]

// Isolated spike: does Dapper.AOT work with Microsoft.Data.Sqlite under
// Native AOT, and can it map the types the control plane actually uses?

var failures = 0;
void Check(string name, bool ok, string? detail = null)
{
    Console.WriteLine($"{(ok ? "PASS" : "FAIL")}  {name}{(detail is null ? "" : $"  [{detail}]")}");
    if (!ok)
    {
        failures++;
    }
}

var dbPath = Path.Combine(Path.GetTempPath(), $"dapper-sqlite-spike-{Guid.NewGuid():N}.db");
var csb = new SqliteConnectionStringBuilder
{
    DataSource = dbPath,
    Mode = SqliteOpenMode.ReadWriteCreate,
    Pooling = true,
    ForeignKeys = true,
    DefaultTimeout = 5,
};
var connectionString = csb.ToString();

static async Task ExecAsync(SqliteConnection connection, string sql)
{
    await using var command = connection.CreateCommand();
    command.CommandText = sql;
    await command.ExecuteNonQueryAsync();
}

static async Task<SqliteConnection> OpenAsync(string connectionString)
{
    var connection = new SqliteConnection(connectionString);
    await connection.OpenAsync();
    return connection;
}

await using (var setup = await OpenAsync(connectionString))
{
    await ExecAsync(setup, "PRAGMA journal_mode=WAL;");
    await ExecAsync(setup, "PRAGMA synchronous=NORMAL;");
    await ExecAsync(setup, """
        CREATE TABLE environments (
            name       TEXT PRIMARY KEY,
            created_at TEXT NOT NULL
        );
        CREATE TABLE agents (
            id             TEXT PRIMARY KEY,
            name           TEXT NOT NULL UNIQUE,
            environment    TEXT NOT NULL REFERENCES environments(name),
            abi_versions   TEXT NOT NULL DEFAULT '[]',
            labels         TEXT NOT NULL DEFAULT '{}',
            status         TEXT NOT NULL,
            registered_at  TEXT NOT NULL,
            last_seen_at   TEXT NOT NULL
        );
        CREATE TABLE deployments (
            id          TEXT PRIMARY KEY,
            environment TEXT NOT NULL,
            status      TEXT NOT NULL,
            created_at  TEXT NOT NULL,
            completed_at TEXT
        );
        CREATE TABLE deployment_waves (
            deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
            "index"       INTEGER NOT NULL CHECK ("index" >= 0),
            selector      TEXT NOT NULL DEFAULT '*',
            status        TEXT NOT NULL,
            started_at    TEXT,
            completed_at  TEXT,
            PRIMARY KEY (deployment_id, "index")
        );
        CREATE TABLE audit_events (
            id         INTEGER PRIMARY KEY AUTOINCREMENT,
            at         TEXT NOT NULL,
            actor      TEXT NOT NULL,
            action     TEXT NOT NULL,
            target     TEXT NOT NULL,
            details    TEXT,
            request_id TEXT
        );
        """);
}

await using (var connection = await OpenAsync(connectionString))
{
    var version = await connection.ExecuteScalarAsync<string?>("SELECT sqlite_version()");
    Check("sqlite native library loaded", !string.IsNullOrEmpty(version), version);

    var now = DateTimeOffset.UtcNow;
    var createdAt = now.ToString("O", CultureInfo.InvariantCulture);

    await connection.ExecuteAsync(
        "INSERT INTO environments (name, created_at) VALUES (@name, @createdAt)",
        new { name = "prod", createdAt });

    var agentId = Guid.NewGuid();
    var registeredAt = now.AddMinutes(-5).ToString("O", CultureInfo.InvariantCulture);
    var lastSeenAt = now.ToString("O", CultureInfo.InvariantCulture);

    var inserted = await connection.ExecuteAsync(
        """
        INSERT INTO agents (id, name, environment, abi_versions, labels, status,
                            registered_at, last_seen_at)
        VALUES (@id, @name, @environment, @abiVersions, @labels, @status,
                @registeredAt, @lastSeenAt)
        """,
        new
        {
            id = agentId,
            name = "dp-01",
            environment = "prod",
            abiVersions = "[\"4\"]",
            labels = "{\"role\":\"demo\"}",
            status = AgentStatus.Online.ToString(),
            registeredAt,
            lastSeenAt,
        });
    Check("insert with Guid/string parameters", inserted == 1, $"rows={inserted}");

    var byName = await connection.QuerySingleOrDefaultAsync<AgentRow>(
        "SELECT id, name, environment, abi_versions, labels, status, registered_at, last_seen_at FROM agents WHERE name = @name",
        new { name = "dp-01" });
    Check("query single + Guid mapping", byName is { Id: var id } && id == agentId);
    Check("enum mapping from TEXT", byName?.Status == AgentStatus.Online, byName?.Status.ToString());
    Check(
        "DateTime mapping from offset text (kind is informational)",
        byName is not null,
        $"{byName?.RegisteredAt:O} kind={byName?.RegisteredAt.Kind}");
    Check("JSON column read as text", byName?.Labels == "{\"role\":\"demo\"}", byName?.Labels);
    Check("array column read as text", byName?.AbiVersions == "[\"4\"]", byName?.AbiVersions);

    var rawTimestamp = await connection.ExecuteScalarAsync<string?>(
        "SELECT registered_at FROM agents WHERE id = @id", new { id = agentId });
    Check("timestamp stored as ISO text", rawTimestamp == registeredAt, rawTimestamp);

    var offsetRaw = await connection.QuerySingleAsync<string>(
        "SELECT registered_at FROM agents WHERE id = @id", new { id = agentId });
    var parsedOffset = DateTimeOffset.Parse(offsetRaw, CultureInfo.InvariantCulture, DateTimeStyles.RoundtripKind);
    Check(
        "explicit DateTimeOffset parse",
        parsedOffset.ToUniversalTime() == now.AddMinutes(-5),
        parsedOffset.ToString("O", CultureInfo.InvariantCulture));

    var offsetMappingFailed = false;
    try
    {
        _ = await connection.QuerySingleAsync<OffsetRow>(
            "SELECT id, registered_at FROM agents WHERE id = @id", new { id = agentId });
    }
    catch (InvalidCastException)
    {
        offsetMappingFailed = true;
    }

    Check("DateTimeOffset direct mapping is unsupported (expected)", offsetMappingFailed);

    await ExecAsync(connection, """
        CREATE TABLE time_probe (
            id     TEXT PRIMARY KEY,
            at_z   TEXT NOT NULL,
            at_ms  INTEGER NOT NULL
        );
        """);
    var atZ = now.UtcDateTime.ToString("O", CultureInfo.InvariantCulture);
    var atMs = now.ToUnixTimeMilliseconds();
    await connection.ExecuteAsync(
        "INSERT INTO time_probe (id, at_z, at_ms) VALUES (@id, @atZ, @atMs)",
        new { id = agentId, atZ, atMs });
    var timeRow = await connection.QuerySingleAsync<TimeRow>(
        "SELECT at_z, at_ms FROM time_probe WHERE id = @id", new { id = agentId });
    Check(
        "DateTime from Z text keeps the instant but reports Local kind (use epoch instead)",
        timeRow.AtZ.ToUniversalTime() == now.UtcDateTime,
        $"{timeRow.AtZ:O} kind={timeRow.AtZ.Kind}");
    Check("epoch milliseconds mapping", timeRow.AtMs == now.ToUnixTimeMilliseconds(), timeRow.AtMs.ToString(CultureInfo.InvariantCulture));
    Check(
        "epoch milliseconds round-trip",
        DateTimeOffset.FromUnixTimeMilliseconds(timeRow.AtMs) ==
        DateTimeOffset.FromUnixTimeMilliseconds(now.ToUnixTimeMilliseconds()),
        DateTimeOffset.FromUnixTimeMilliseconds(timeRow.AtMs).ToString("O", CultureInfo.InvariantCulture));


    var jsonRole = await connection.ExecuteScalarAsync<string?>(
        "SELECT json_extract(labels, '$.role') FROM agents WHERE id = @id",
        new { id = agentId });
    Check("SQLite JSON1 functions", jsonRole == "demo", jsonRole);

    var exists = await connection.ExecuteScalarAsync<bool>(
        "SELECT EXISTS (SELECT 1 FROM agents WHERE id = @id)",
        new { id = agentId });
    Check("EXISTS maps to bool", exists);

    var updated = await connection.ExecuteAsync(
        """
        INSERT INTO agents (id, name, environment, abi_versions, labels, status,
                            registered_at, last_seen_at)
        VALUES (@id, @name, @environment, @abiVersions, @labels, @status,
                @registeredAt, @lastSeenAt)
        ON CONFLICT (id) DO UPDATE SET
            name = excluded.name,
            status = excluded.status,
            last_seen_at = excluded.last_seen_at
        """,
        new
        {
            id = agentId,
            name = "dp-01",
            environment = "prod",
            abiVersions = "[\"4\"]",
            labels = "{\"role\":\"demo\"}",
            status = AgentStatus.Offline.ToString(),
            registeredAt,
            lastSeenAt,
        });
    Check("upsert with ON CONFLICT", updated == 1, $"rows={updated}");

    var secondAgentId = Guid.NewGuid();
    await connection.ExecuteAsync(
        "INSERT INTO agents (id, name, environment, abi_versions, labels, status, registered_at, last_seen_at) " +
        "VALUES (@id, @name, 'prod', '[]', '{}', 'Online', @at, @at)",
        new { id = secondAgentId, name = "dp-02", at = lastSeenAt });

    var ids = new[] { agentId, secondAgentId, Guid.NewGuid() };
    var found = (await connection.QueryAsync<Guid>(
        "SELECT id FROM agents WHERE id IN @ids", new { ids })).ToList();
    Check("Dapper list expansion (IN @ids)", found.Count == 2, $"found={found.Count}");

    var limited = (await connection.QueryAsync<AgentRow>(
        "SELECT id, name, environment, abi_versions, labels, status, registered_at, last_seen_at " +
        "FROM agents ORDER BY name LIMIT @limit", new { limit = 1 })).ToList();
    Check("LIMIT parameter", limited.Count == 1, $"rows={limited.Count}");

    var deploymentId = Guid.NewGuid();
    await using (var transaction = connection.BeginTransaction(IsolationLevel.Serializable, deferred: false))
    {
        await connection.ExecuteAsync(
            "INSERT INTO deployments (id, environment, status, created_at, completed_at) " +
            "VALUES (@id, @environment, @status, @createdAt, NULL)",
            new
            {
                id = deploymentId,
                environment = "prod",
                status = DeploymentStatus.Running.ToString(),
                createdAt,
            },
            transaction);
        await connection.ExecuteAsync(
            "INSERT INTO deployment_waves (deployment_id, \"index\", selector, status, started_at, completed_at) " +
            "VALUES (@deploymentId, @index, @selector, @status, @startedAt, NULL)",
            new
            {
                deploymentId,
                index = 0,
                selector = "*",
                status = DeploymentStatus.Running.ToString(),
                startedAt = createdAt,
            },
            transaction);
        await transaction.CommitAsync();
    }

    var deployment = await connection.QuerySingleOrDefaultAsync<DeploymentRow>(
        "SELECT id, environment, status, created_at, completed_at FROM deployments WHERE id = @id",
        new { id = deploymentId });
    Check("deployment insert/query", deployment is { Id: var deploymentResultId } && deploymentResultId == deploymentId);
    Check("nullable DateTime mapping", deployment is { CompletedAt: null });

    var waveCount = await connection.ExecuteScalarAsync<long>(
        "SELECT COUNT(*) FROM deployment_waves WHERE deployment_id = @id AND \"index\" = 0",
        new { id = deploymentId });
    Check("quoted reserved column \"index\"", waveCount == 1, $"rows={waveCount}");

    var auditBefore = await connection.ExecuteScalarAsync<long>("SELECT COUNT(*) FROM audit_events");
    await using (var transaction = connection.BeginTransaction(IsolationLevel.Serializable, deferred: false))
    {
        await connection.ExecuteAsync(
            "INSERT INTO audit_events (at, actor, action, target, details, request_id) " +
            "VALUES (@at, @actor, @action, @target, @details, @requestId)",
            new
            {
                at = createdAt,
                actor = "spike",
                action = "rollback",
                target = "agent",
                details = "{}",
                requestId = (string?)null,
            },
            transaction);
        await transaction.RollbackAsync();
    }

    var auditAfter = await connection.ExecuteScalarAsync<long>("SELECT COUNT(*) FROM audit_events");
    Check("transaction rollback", auditAfter == auditBefore, $"before={auditBefore} after={auditAfter}");
}

// Concurrent writers: WAL + busy_timeout should serialize them without
// "database is locked" failures.
var workers = 4;
var perWorker = 50;
await Task.WhenAll(Enumerable.Range(0, workers).Select(async worker =>
{
    await using var connection = await OpenAsync(connectionString);
    for (var i = 0; i < perWorker; i++)
    {
        await connection.ExecuteAsync(
            "INSERT INTO audit_events (at, actor, action, target, details, request_id) " +
            "VALUES (@at, @actor, @action, @target, NULL, NULL)",
            new
            {
                at = DateTimeOffset.UtcNow.ToString("O", CultureInfo.InvariantCulture),
                actor = $"worker-{worker}",
                action = "spike",
                target = i.ToString(CultureInfo.InvariantCulture),
            });
    }
}));

await using (var connection = await OpenAsync(connectionString))
{
    var total = await connection.ExecuteScalarAsync<long>("SELECT COUNT(*) FROM audit_events");
    Check("concurrent writers (WAL)", total == workers * perWorker, $"rows={total}");

    var pageCount = await connection.ExecuteScalarAsync<long>("PRAGMA page_count");
    var pageSize = await connection.ExecuteScalarAsync<long>("PRAGMA page_size");
    Check("database file created", File.Exists(dbPath), $"~{pageCount * pageSize / 1024} KiB");
}

Console.WriteLine();
Console.WriteLine(failures == 0 ? "SPIKE RESULT: PASS" : $"SPIKE RESULT: FAIL ({failures} checks)");
if (failures != 0)
{
    Environment.ExitCode = 1;
}

try
{
    File.Delete(dbPath);
    File.Delete(dbPath + "-wal");
    File.Delete(dbPath + "-shm");
}
catch
{
    // Diagnostics are more important than temp-file cleanup.
}

internal enum AgentStatus
{
    Pending,
    Online,
    Offline,
}

internal enum DeploymentStatus
{
    Pending,
    Running,
    Succeeded,
    Failed,
}

internal sealed class AgentRow
{
    public Guid Id { get; set; }

    public string Name { get; set; } = string.Empty;

    public string Environment { get; set; } = string.Empty;

    public string AbiVersions { get; set; } = "[]";

    public string Labels { get; set; } = "{}";

    public AgentStatus Status { get; set; }

    public DateTime RegisteredAt { get; set; }

    public DateTime LastSeenAt { get; set; }
}

internal sealed class DeploymentRow
{
    public Guid Id { get; set; }

    public string Environment { get; set; } = string.Empty;

    public DeploymentStatus Status { get; set; }

    public DateTime CreatedAt { get; set; }

    public DateTime? CompletedAt { get; set; }
}

internal sealed class OffsetRow
{
    public Guid Id { get; set; }

    public DateTimeOffset RegisteredAt { get; set; }
}

internal sealed class TimeRow
{
    public DateTime AtZ { get; set; }

    public long AtMs { get; set; }
}
