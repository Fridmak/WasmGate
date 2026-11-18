using System.Globalization;
using System.Text.Json;
using Microsoft.Data.Sqlite;
using Npgsql;
using WasmGate.Control.Api.Observability;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Artifacts;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Infrastructure.Security;
using WasmGate.Control.Infrastructure.Storage;
using WasmGate.Control.Infrastructure.Validation;

namespace WasmGate.Control.Api;

/// <summary>
/// Builds runtime options and registers the control-plane services. Kept as an
/// extension block so <c>Program.cs</c> stays a short composition root and the
/// dependency graph remains explicit and AOT-friendly.
/// </summary>
public static class StartUp
{
    /// <param name="builder">Application builder whose host and services are configured.</param>
    extension(IHostApplicationBuilder builder)
    {
        /// <summary>Replaces the default logging with structured JSON to stdout.</summary>
        public void SetupLogging()
        {
            builder.Logging.ClearProviders();
            builder.Logging.AddJsonConsole(options =>
            {
                options.IncludeScopes = true;
                options.TimestampFormat = "yyyy-MM-ddTHH:mm:ss.fffZ";
                options.UseUtcTimestamp = true;
                options.JsonWriterOptions = new JsonWriterOptions
                {
                    Indented = false
                };
            });
        }

        /// <summary>Reads control-plane options from <c>WASMGATE_CP_*</c> environment variables.</summary>
        /// <returns>Options with defaults applied for every unset variable.</returns>
        /// <remarks>
        /// Durations use the invariant <see cref="TimeSpan"/> format. Invalid values
        /// silently fall back to the default, so a typo does not stop the process.
        /// </remarks>
        public static ControlPlaneOptions GetCpOptions()
        {
            var postgres = Env("WASMGATE_CP_POSTGRES");
            var sqlitePath = Env("WASMGATE_CP_SQLITE_PATH");
            var storage = ParseStorage(Env("WASMGATE_CP_STORAGE"), postgres, sqlitePath);

            return new ControlPlaneOptions
            {
                Storage = storage,
                SqlitePath = sqlitePath ?? "data/control.db",
                PostgresConnectionString = postgres ?? ControlPlaneOptions.DefaultPostgresConnectionString,
                GrpcListen = Env("WASMGATE_CP_GRPC_LISTEN") ?? "http://127.0.0.1:9096",
                HttpListen = Env("WASMGATE_CP_HTTP_LISTEN") ?? "http://127.0.0.1:9097",
                ArtifactRoot = Env("WASMGATE_CP_ARTIFACT_ROOT") ?? "data/artifacts",
                WasmgateBinary = Env("WASMGATE_CP_WASMGATE_BIN") ?? "wasmgate",
                SigningKeyPath = Env("WASMGATE_CP_SIGNING_KEY"),
                ValidationTimeout = EnvDuration("WASMGATE_CP_VALIDATE_TIMEOUT", TimeSpan.FromSeconds(60)),
                AgentCaCertPath = Env("WASMGATE_CP_AGENT_CA_CERT") ?? "data/ca/agents-ca.crt",
                AgentCaKeyPath = Env("WASMGATE_CP_AGENT_CA_KEY") ?? "data/ca/agents-ca.key",
                AgentCertificateLifetime = EnvDuration("WASMGATE_CP_AGENT_CERT_TTL", TimeSpan.FromDays(30)),
                AdminToken = Env("WASMGATE_CP_ADMIN_TOKEN"),
                ApplyTimeout = EnvDuration("WASMGATE_CP_APPLY_TIMEOUT", TimeSpan.FromMinutes(5)),
                DeploymentTimeout = EnvDuration("WASMGATE_CP_DEPLOYMENT_TIMEOUT", TimeSpan.FromMinutes(15)),
                MaxArtifactBytes = EnvLong("WASMGATE_CP_MAX_ARTIFACT_BYTES", 64L * 1024 * 1024),
                MaxConfigBytes = (int)EnvLong("WASMGATE_CP_MAX_CONFIG_BYTES", 1024 * 1024),
                GrpcTls = new ControlPlaneTlsOptions
                {
                    CertificatePath = Env("WASMGATE_CP_GRPC_TLS_CERT"),
                    PrivateKeyPath = Env("WASMGATE_CP_GRPC_TLS_KEY"),
                    ClientCaPath = Env("WASMGATE_CP_GRPC_CLIENT_CA") ?? "data/ca/agents-ca.crt",
                    RequireClientCertificate = EnvBool("WASMGATE_CP_GRPC_REQUIRE_CLIENT_CERT", true),
                },
            };
        }

        /// <summary>
        /// Registers persistence, security and release-workflow services.
        /// </summary>
        /// <remarks>
        /// Every repository is backed by PostgreSQL. The connection string comes
        /// from <see cref="ControlPlaneOptions.PostgresConnectionString"/>, and
        /// the schema from <c>control-plane/migrations</c> must be applied before
        /// the first request (see <c>control-plane/README.md</c>).
        /// The session registry is the only in-memory registration here: it holds
        /// live connections, not data.
        /// </remarks>
        public void RegisterServices(ControlPlaneOptions options)
        {
            if (options.Storage == StorageProvider.Sqlite)
            {
                var connectionString = new SqliteConnectionStringBuilder
                {
                    DataSource = Path.GetFullPath(options.SqlitePath),
                    Mode = SqliteOpenMode.ReadWriteCreate,
                    Pooling = true,
                    ForeignKeys = true,
                    DefaultTimeout = 5,
                }.ToString();

                builder.Services.AddSingleton(new SqliteConnectionFactory(connectionString));
                builder.Services.AddSingleton<SqliteInstanceLock>();
                builder.Services.AddSingleton<IStorageInitializer, SqliteStorageInitializer>();
                builder.Services.AddSingleton<IStorageProbe, SqliteStorageProbe>();
                builder.Services.AddSingleton<IEnvironmentOperationLock, SqliteEnvironmentOperationLock>();
                builder.Services.AddSingleton<IAgentRepository, SqliteAgentRepository>();
                builder.Services.AddSingleton<IEnvironmentRepository, SqliteEnvironmentRepository>();
                builder.Services.AddSingleton<IConfigVersionRepository, SqliteConfigVersionRepository>();
                builder.Services.AddSingleton<IArtifactRepository, SqliteArtifactRepository>();
                builder.Services.AddSingleton<IReleaseRepository, SqliteReleaseRepository>();
                builder.Services.AddSingleton<IDeploymentRepository, SqliteDeploymentRepository>();
                builder.Services.AddSingleton<IAuditLog, SqliteAuditLog>();
                builder.Services.AddSingleton<IEnrollmentTokenRepository, SqliteEnrollmentTokenRepository>();
                builder.Services.AddSingleton<IAgentCertificateRepository, SqliteAgentCertificateRepository>();
            }
            else
            {
                builder.Services.AddSingleton(_ =>
                    new NpgsqlDataSourceBuilder(options.PostgresConnectionString).Build());
                builder.Services.AddSingleton<IStorageInitializer, PostgresStorageInitializer>();
                builder.Services.AddSingleton<IStorageProbe, PostgresStorageProbe>();
                builder.Services.AddSingleton<IEnvironmentOperationLock, PostgresEnvironmentOperationLock>();
                builder.Services.AddSingleton<IAgentRepository, PostgresAgentRepository>();
                builder.Services.AddSingleton<IEnvironmentRepository, PostgresEnvironmentRepository>();
                builder.Services.AddSingleton<IConfigVersionRepository, PostgresConfigVersionRepository>();
                builder.Services.AddSingleton<IArtifactRepository, PostgresArtifactRepository>();
                builder.Services.AddSingleton<IReleaseRepository, PostgresReleaseRepository>();
                builder.Services.AddSingleton<IDeploymentRepository, PostgresDeploymentRepository>();
                builder.Services.AddSingleton<IAuditLog, PostgresAuditLog>();
                builder.Services.AddSingleton<IEnrollmentTokenRepository, PostgresEnrollmentTokenRepository>();
                builder.Services.AddSingleton<IAgentCertificateRepository, PostgresAgentCertificateRepository>();
            }

            builder.Services.AddSingleton<ControlMetrics>();
            builder.Services.AddSingleton<IArtifactStore, FileSystemArtifactStore>();
            builder.Services.AddSingleton<IConfigValidator, GoConfigValidator>();
            builder.Services.AddSingleton<IReleaseSigner, EcdsaReleaseSigner>();
            builder.Services.AddSingleton<IAgentSessionRegistry, InMemoryAgentSessionRegistry>();
            builder.Services.AddSingleton<ICertificateAuthority>(new PkiCertificateAuthority(options));
            builder.Services.AddSingleton<IAgentProvisioningService, AgentProvisioningService>();
            builder.Services.AddSingleton<IReleaseService, ReleaseService>();
        }
    }

    /// <summary>Selects the storage provider from the environment.</summary>
    /// <param name="raw">Value of <c>WASMGATE_CP_STORAGE</c>, or null.</param>
    /// <param name="postgres">Value of <c>WASMGATE_CP_POSTGRES</c>, or null.</param>
    /// <param name="sqlitePath">Value of <c>WASMGATE_CP_SQLITE_PATH</c>, or null.</param>
    /// <returns>The selected provider.</returns>
    /// <remarks>
    /// An unset <c>WASMGATE_CP_STORAGE</c> keeps the previous behaviour:
    /// PostgreSQL when a connection string is present, SQLite when only a
    /// SQLite path is present, and PostgreSQL by default. SQLite plus an
    /// explicit PostgreSQL connection string is rejected as ambiguous.
    /// </remarks>
    internal static StorageProvider ParseStorage(string? raw, string? postgres, string? sqlitePath)
    {
        if (string.IsNullOrWhiteSpace(raw))
        {
            if (postgres is not null)
            {
                return StorageProvider.Postgres;
            }

            return sqlitePath is not null ? StorageProvider.Sqlite : StorageProvider.Postgres;
        }

        switch (raw.Trim().ToLowerInvariant())
        {
            case "postgres":
                return StorageProvider.Postgres;
            case "sqlite":
                if (postgres is not null)
                {
                    throw new InvalidOperationException(
                        "WASMGATE_CP_STORAGE=sqlite cannot be combined with WASMGATE_CP_POSTGRES. " +
                        "Unset the PostgreSQL connection string or select storage=postgres.");
                }

                return StorageProvider.Sqlite;
            default:
                throw new InvalidOperationException(
                    $"Unknown WASMGATE_CP_STORAGE value '{raw}'. Accepted values: postgres, sqlite.");
        }
    }

    /// <summary>Reads a non-empty environment variable.</summary>
    /// <param name="name">Environment variable name.</param>
    /// <returns>The value, or null when the variable is unset or blank.</returns>
    private static string? Env(string name)
    {
        var value = Environment.GetEnvironmentVariable(name);
        return string.IsNullOrWhiteSpace(value) ? null : value;
    }

    /// <summary>Reads a boolean environment variable, falling back to a default.</summary>
    /// <param name="name">Environment variable name.</param>
    /// <param name="defaultValue">Value used when the variable is unset or invalid.</param>
    /// <returns>Parsed boolean value or <paramref name="defaultValue"/>.</returns>
    private static bool EnvBool(string name, bool defaultValue)
    {
        var value = Env(name);
        return bool.TryParse(value, out var parsed) ? parsed : defaultValue;
    }

    /// <summary>Reads a duration environment variable, falling back to a default.</summary>
    /// <param name="name">Environment variable name.</param>
    /// <param name="defaultValue">Value used when the variable is unset or invalid.</param>
    /// <remarks>
    /// Accepts the invariant <see cref="TimeSpan"/> format, for example
    /// <c>30.00:00:00</c> for 30 days or <c>00:05:00</c> for five minutes.
    /// </remarks>
    /// <returns>Parsed duration or <paramref name="defaultValue"/>.</returns>
    private static TimeSpan EnvDuration(string name, TimeSpan defaultValue)
    {
        var value = Env(name);
        return TimeSpan.TryParse(value, CultureInfo.InvariantCulture, out var parsed) ? parsed : defaultValue;
    }

    /// <summary>Reads a positive integer environment variable, falling back to a default.</summary>
    /// <param name="name">Environment variable name.</param>
    /// <param name="defaultValue">Value used when the variable is unset, invalid or not positive.</param>
    /// <returns>Parsed value or <paramref name="defaultValue"/>.</returns>
    private static long EnvLong(string name, long defaultValue)
    {
        var value = Env(name);
        return long.TryParse(value, NumberStyles.Integer, CultureInfo.InvariantCulture, out var parsed) &&
               parsed > 0
            ? parsed
            : defaultValue;
    }
}
