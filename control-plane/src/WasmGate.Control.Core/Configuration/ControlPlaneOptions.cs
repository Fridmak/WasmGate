namespace WasmGate.Control.Core.Configuration;

/// <summary>
/// Runtime options. Values are read from environment variables in Program.cs;
/// this type deliberately has no dependency on the configuration binder, which
/// keeps the AOT graph small.
/// </summary>
/// <remarks>
/// Filesystem paths without a root are relative to the control-plane process
/// working directory (systemd WorkingDirectory, container WORKDIR or the shell
/// directory used to start the process).
/// </remarks>
public sealed class ControlPlaneOptions
{
    /// <summary>
    /// Connection string used when <see cref="PostgresConnectionString"/> is not set.
    /// Port 6767 is used instead of the default 5432 so that a locally installed
    /// PostgreSQL service cannot shadow the development container.
    /// </summary>
    public const string DefaultPostgresConnectionString =
        "Host=127.0.0.1;Port=6767;Database=wasmgate;Username=wasmgate;Password=wasmgate";

    /// <summary>
    /// Npgsql connection string for the control-plane database.
    /// </summary>
    public string PostgresConnectionString { get; init; } = DefaultPostgresConnectionString;

    /// <summary>
    /// Agent gRPC endpoint. HTTP/2 only.
    /// </summary>
    /// <remarks>
    /// Use <c>http://&lt;ip|localhost|*&gt;:&lt;port&gt;</c>,
    /// <c>https://&lt;ip|localhost|*&gt;:&lt;port&gt;</c> or
    /// <c>unix:///absolute/path.sock</c>. A port is required. TLS is configured
    /// through <see cref="GrpcTls"/>.
    /// </remarks>
    public string GrpcListen { get; init; } = "http://127.0.0.1:9096";

    /// <summary>TLS and mutual-TLS settings for the agent gRPC endpoint.</summary>
    public ControlPlaneTlsOptions GrpcTls { get; init; } = new();

    /// <summary>
    /// Admin/health endpoint. HTTP/1.1 only.
    /// </summary>
    /// <remarks>
    /// Use <c>http://&lt;ip|localhost|*&gt;:&lt;port&gt;</c> or
    /// <c>unix:///absolute/path.sock</c>. A port is required. TLS is not
    /// configured on this endpoint; terminate TLS externally or add it later.
    /// </remarks>
    public string HttpListen { get; init; } = "http://127.0.0.1:9097";

    /// <summary>Directory used by the local content-addressed artifact store.</summary>
    /// <remarks>
    /// Absolute path, or relative to the control-plane process working directory.
    /// The directory must be writable by the control-plane process.
    /// </remarks>
    public string ArtifactRoot { get; init; } = "data/artifacts";

    /// <summary>Path or command name of the Go data-plane binary used for validation.</summary>
    /// <remarks>
    /// An absolute path is used as-is. A relative path is resolved against the
    /// control-plane process working directory. A bare command name such as
    /// "wasmgate" is looked up through PATH. Use the same version as the target
    /// data plane so validation matches the runtime.
    /// </remarks>
    public string WasmgateBinary { get; init; } = "wasmgate";

    /// <summary>Optional path to the ECDSA P-256 private key used to sign release manifests.</summary>
    /// <remarks>
    /// Absolute path, or relative to the control-plane process working directory.
    /// When the file is missing, a new P-256 key is generated at this path and
    /// the matching public key is written next to it as <c>&lt;path&gt;.pub</c>.
    /// The private key is written with owner-only permissions on Unix.
    /// </remarks>
    public string? SigningKeyPath { get; init; }

    /// <summary>Maximum time allowed for one <c>wasmgate -validate</c> invocation.</summary>
    /// <remarks>
    /// A hung validator must not keep an admin request open forever. The
    /// validator process is killed when the timeout elapses.
    /// </remarks>
    public TimeSpan ValidationTimeout { get; init; } = TimeSpan.FromSeconds(60);

    /// <summary>PEM certificate of the agent CA used to sign data-plane certificates.</summary>
    public string AgentCaCertPath { get; init; } = "data/ca/agents-ca.crt";

    /// <summary>PEM private key of the agent CA. Stored with restrictive permissions.</summary>
    public string AgentCaKeyPath { get; init; } = "data/ca/agents-ca.key";

    /// <summary>Validity period of issued agent certificates.</summary>
    public TimeSpan AgentCertificateLifetime { get; init; } = TimeSpan.FromDays(30);
}
