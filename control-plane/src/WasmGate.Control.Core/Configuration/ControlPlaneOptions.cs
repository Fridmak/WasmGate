namespace WasmGate.Control.Core.Configuration;

/// <summary>
/// Runtime options. Values are read from environment variables by
/// <c>StartUp.GetCpOptions</c>; this type deliberately has no dependency on the
/// configuration binder, which keeps the AOT graph small.
/// </summary>
/// <remarks>
/// Filesystem paths without a root are relative to the control-plane process
/// working directory (systemd WorkingDirectory, container WORKDIR or the shell
/// directory used to start the process).
/// </remarks>
public sealed class ControlPlaneOptions
{
    /// <summary>
    /// SQLite database file used by the control plane.
    /// </summary>
    /// <remarks>
    /// A relative path is resolved against the control-plane process working
    /// directory, matching the other path options. The parent directory is
    /// created on startup.
    /// </remarks>
    public string SqlitePath { get; init; } = "data/control.db";

    /// <summary>
    /// Trusted loopback mode for a control plane and data plane on one machine.
    /// </summary>
    /// <remarks>
    /// Local mode requires plaintext loopback listeners and never issues agent
    /// certificates. A data plane connects with <c>-cp-local</c> and no enrollment
    /// token. It trusts every process that can reach the loopback listeners, so
    /// use it only on a single-user host; for a shared host or a fleet use
    /// TLS/mTLS and enrollment tokens.
    /// </remarks>
    public bool Local { get; init; }

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

    /// <summary>
    /// Root directory for short-lived SQLite backup workspaces.
    /// </summary>
    /// <remarks>
    /// Each download creates a private subdirectory below this root and removes
    /// it when the response completes. Null means the system temporary
    /// directory. Point it at a private volume in production so snapshots never
    /// land in a world-readable location.
    /// </remarks>
    public string? BackupTempDirectory { get; init; }

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
    /// <remarks>
    /// Renewal is automatic: a data plane renews its certificate over mTLS before
    /// this period ends, so the value bounds the exposure of a leaked key rather
    /// than the lifetime of an agent.
    /// </remarks>
    public TimeSpan AgentCertificateLifetime { get; init; } = TimeSpan.FromDays(30);

    /// <summary>
    /// Bearer token required by the admin API when it listens on a non-loopback
    /// address.
    /// </summary>
    /// <remarks>
    /// Null or empty means "no authentication", which is accepted only for a
    /// loopback or Unix-socket listener. The control plane refuses to start on a
    /// non-loopback HTTP address without a token.
    /// </remarks>
    public string? AdminToken { get; init; }

    /// <summary>Time an agent has to apply a release before the apply times out.</summary>
    /// <remarks>
    /// The value is sent with every <c>ApplyRelease</c>. It must cover the
    /// download of the release artifacts on slow links; the default is generous
    /// so a large plugin does not fail on a poor connection.
    /// </remarks>
    public TimeSpan ApplyTimeout { get; init; } = TimeSpan.FromMinutes(5);

    /// <summary>
    /// Time after which a deployment that is still running is marked failed by the
    /// background reaper.
    /// </summary>
    /// <remarks>
    /// Covers the cases a result cannot: a control plane crash between storing the
    /// deployment and dispatching it, or an agent that never reports back.
    /// </remarks>
    public TimeSpan DeploymentTimeout { get; init; } = TimeSpan.FromMinutes(15);

    /// <summary>Largest artifact accepted by the admin API, in bytes.</summary>
    /// <remarks>
    /// Must stay below what the data plane accepts. Uploads are base64-encoded in
    /// JSON, so the request body limit is derived from this value.
    /// </remarks>
    public long MaxArtifactBytes { get; init; } = 64L * 1024 * 1024;

    /// <summary>Largest configuration document accepted by the admin API, in bytes.</summary>
    public int MaxConfigBytes { get; init; } = 1024 * 1024;
}
