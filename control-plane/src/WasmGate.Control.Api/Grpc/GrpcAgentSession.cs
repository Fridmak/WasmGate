using Grpc.Core;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.V1;
using CoreArtifactKind = WasmGate.Control.Core.Entities.Releases.ArtifactKind;
using ProtoArtifactKind = WasmGate.Control.V1.ArtifactKind;

namespace WasmGate.Control.Api.Grpc;

/// <summary>
/// One live gRPC control stream, created by <see cref="AgentControlService.Session"/>
/// for a single agent connection.
/// </summary>
/// <remarks>
/// Writes are serialized, because a gRPC response stream allows only one writer
/// at a time. <see cref="CloseAsync"/> cancels the linked token, which stops the
/// read loop of the owning call; the call itself still unregisters the session.
/// </remarks>
public sealed class GrpcAgentSession : IAgentSession
{
    /// <summary>Upper bound for one server-to-agent write, including flow-control backpressure.</summary>
    private static readonly TimeSpan SendTimeout = TimeSpan.FromSeconds(10);

    private readonly IServerStreamWriter<ControlMessage> _writer;
    private readonly CancellationTokenSource _lifetime;
    private readonly SemaphoreSlim _writeSlim = new(1, 1);
    private int _closed;

    /// <summary>Creates a session bound to one gRPC call.</summary>
    /// <param name="agentId">Identifier parsed from the agent's Hello.</param>
    /// <param name="writer">Response stream of the gRPC call.</param>
    /// <param name="context">Context of the current gRPC call.</param>
    public GrpcAgentSession(
        Guid agentId,
        IServerStreamWriter<ControlMessage> writer,
        ServerCallContext context)
    {
        AgentId = agentId;
        _writer = writer;
        _lifetime = CancellationTokenSource.CreateLinkedTokenSource(context.CancellationToken);
    }

    /// <inheritdoc />
    public Guid AgentId { get; }

    /// <summary>
    /// Token cancelled when the gRPC call ends or another session takes over.
    /// The read loop in <c>Session</c> must use this token.
    /// </summary>
    public CancellationToken Token => _lifetime.Token;

    /// <inheritdoc />
    public async ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken)
    {
        var message = ToControlMessage(command);
        using var timeout = new CancellationTokenSource(SendTimeout);
        using var linked = CancellationTokenSource.CreateLinkedTokenSource(
            _lifetime.Token,
            cancellationToken,
            timeout.Token);

        try
        {
            await _writeSlim.WaitAsync(linked.Token);
            try
            {
                await _writer.WriteAsync(message, linked.Token);
            }
            finally
            {
                _writeSlim.Release();
            }
        }
        catch (OperationCanceledException) when (timeout.IsCancellationRequested)
        {
            // The agent accepted the stream but stopped reading. Keeping the
            // session would block every future deployment behind this write.
            _lifetime.Cancel();
            throw;
        }
    }

    /// <inheritdoc />
    public ValueTask CloseAsync(CancellationToken cancellationToken)
    {
        // Cancel the linked token so the session's read loop and any in-flight
        // writes stop.
        if (Interlocked.Exchange(ref _closed, 1) == 0)
        {
            _lifetime.Cancel();
        }

        return ValueTask.CompletedTask;
    }

    /// <summary>Maps one artifact reference to its protobuf representation.</summary>
    /// <param name="artifact">Artifact reference from the application layer.</param>
    private static ArtifactRef ToArtifactRef(AgentArtifact artifact) => new()
    {
        Id = artifact.Id,
        Name = artifact.Name,
        Kind = artifact.Kind switch
        {
            CoreArtifactKind.Wasm => ProtoArtifactKind.Wasm,
            CoreArtifactKind.Certificate => ProtoArtifactKind.Certificate,
            CoreArtifactKind.Key => ProtoArtifactKind.Key,
            CoreArtifactKind.Other => ProtoArtifactKind.Other,
            _ => ProtoArtifactKind.Unspecified,
        },
        Size = artifact.Size,
        Signature = artifact.Signature ?? string.Empty,
    };

    /// <summary>Maps a transport-neutral command to a protobuf control message.</summary>
    /// <param name="command">Command received from the application layer.</param>
    /// <returns>Message that can be written to the agent stream.</returns>
    /// <exception cref="ArgumentOutOfRangeException">The command kind is unknown.</exception>
    private static ControlMessage ToControlMessage(AgentCommand command) => command.Kind switch
    {
        AgentCommandKind.ApplyRelease => new ControlMessage
        {
            ApplyRelease = new ApplyRelease
            {
                ReleaseId = command.ReleaseId ?? string.Empty,
                ConfigSha256 = command.ConfigSha256 ?? string.Empty,
                ConfigYaml = command.ConfigYaml is null
                    ? Google.Protobuf.ByteString.Empty
                    : Google.Protobuf.ByteString.CopyFromUtf8(command.ConfigYaml),
                TimeoutMs = command.TimeoutMs,
                DeploymentId = command.DeploymentId ?? string.Empty,
                Artifacts = { command.Artifacts?.Select(ToArtifactRef) ?? [] },
                Manifest = command.Manifest is null
                    ? Google.Protobuf.ByteString.Empty
                    : Google.Protobuf.ByteString.CopyFrom(command.Manifest),
                ManifestSignature = command.ManifestSignature is null
                    ? Google.Protobuf.ByteString.Empty
                    : Google.Protobuf.ByteString.CopyFrom(command.ManifestSignature),
            },
        },
        AgentCommandKind.Drain => new ControlMessage
        {
            Drain = new Drain
            {
                TimeoutMs = command.TimeoutMs,
                Reason = command.Reason ?? string.Empty,
            },
        },
        AgentCommandKind.SetLogLevel => new ControlMessage
        {
            SetLogLevel = new SetLogLevel
            {
                Level = command.Reason ?? string.Empty,
            },
        },
        AgentCommandKind.Ping => new ControlMessage
        {
            Ping = new Ping
            {
                Nonce = Guid.NewGuid().ToString("N"),
            },
        },
        AgentCommandKind.Rollback => new ControlMessage
        {
            Rollback = new Rollback
            {
                ReleaseId = command.ReleaseId ?? string.Empty,
                Reason = command.Reason ?? string.Empty,
            },
        },
        _ => throw new ArgumentOutOfRangeException(nameof(command), command.Kind, "Unknown agent command."),
    };
}
