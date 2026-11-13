using System.Text.Json;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Artifacts;
using WasmGate.Control.Infrastructure.Postgres;
using WasmGate.Control.Infrastructure.Security;
using WasmGate.Control.Infrastructure.Validation;

namespace WasmGate.Control.Api;

/// <summary>
/// ????????????? ???? ????????????, ??????????? ??? ?????? ??????????.
/// ??? ?????????? ?????? ???????????? AOT.
/// </summary>
public static class StartUp
{
    /// <param name="builder">??????????? ??????????, ? ???????? ????????????? ???????????.</param>
    extension(IHostApplicationBuilder builder)
    {
        /// <summary>
        /// ????????? ??????????? ? ?????? ??????????.
        /// </summary>
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

        /// <summary>
        /// Get all oprions
        /// </summary>
        public ControlPlaneOptions GetCPOptions()
        {
            return new ControlPlaneOptions
            {
                PostgresConnectionString = Env("WASMGATE_CP_POSTGRES") ?? ControlPlaneOptions.DefaultPostgresConnectionString,
                GrpcListen = Env("WASMGATE_CP_GRPC_LISTEN") ?? "http://127.0.0.1:9096",
                HttpListen = Env("WASMGATE_CP_HTTP_LISTEN") ?? "http://127.0.0.1:9097",
                ArtifactRoot = Env("WASMGATE_CP_ARTIFACT_ROOT") ?? "data/artifacts",
                WasmgateBinary = Env("WASMGATE_CP_WASMGATE_BIN") ?? "wasmgate",
                SigningKeyPath = Env("WASMGATE_CP_SIGNING_KEY"),
                AgentCaCertPath = Env("WASMGATE_CP_AGENT_CA_CERT") ?? "data/ca/agents-ca.crt",
                AgentCaKeyPath = Env("WASMGATE_CP_AGENT_CA_KEY") ?? "data/ca/agents-ca.key",
                AgentCertificateLifetime = EnvDuration("WASMGATE_CP_AGENT_CERT_TTL", TimeSpan.FromDays(30)),
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
        /// Persistence and infrastructure are registered now, but their PostgreSQL
        /// implementations are intentionally TODO stubs.
        /// </summary>
        public void RegisterServices()
        {
            // In-memory implementations for the MVP; swap to PostgreSQL later.
            builder.Services.AddSingleton<IAgentRepository, InMemoryAgentRepository>();
            builder.Services.AddSingleton<IConfigVersionRepository, InMemoryConfigVersionRepository>();
            builder.Services.AddSingleton<IArtifactRepository, InMemoryArtifactRepository>();
            builder.Services.AddSingleton<IReleaseRepository, InMemoryReleaseRepository>();
            builder.Services.AddSingleton<IDeploymentRepository, InMemoryDeploymentRepository>();
            builder.Services.AddSingleton<IAuditLog, InMemoryAuditLog>();
            builder.Services.AddSingleton<IArtifactStore, FileSystemArtifactStore>();
            builder.Services.AddSingleton<IConfigValidator, GoConfigValidator>();
            builder.Services.AddSingleton<IReleaseSigner, EcdsaReleaseSigner>();
            builder.Services.AddSingleton<IAgentSessionRegistry, InMemoryAgentSessionRegistry>();
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
    /// <returns>Parsed duration or <paramref name="defaultValue"/>.</returns>
    private static TimeSpan EnvDuration(string name, TimeSpan defaultValue)
    {
        var value = Env(name);
        return TimeSpan.TryParse(value, out var parsed) ? parsed : defaultValue;
    }
}

