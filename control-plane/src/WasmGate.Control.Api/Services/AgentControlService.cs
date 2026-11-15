using System.Security.Cryptography.X509Certificates;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Services;

/// <summary>
/// Bidirectional control stream for data-plane agents.
/// </summary>
/// <remarks>
/// <para>Flow:</para>
/// <list type="number">
///   <item>read the first message and require a <c>Hello</c>;</item>
///   <item>validate the agent ID with <see cref="TryParseHello"/>;</item>
///   <item>when TLS with client certificates is enabled, compare the certificate identity with the Hello using <see cref="TryGetClientAgentId"/>;</item>
///   <item>persist the Hello fields with <see cref="PersistHelloAsync"/>;</item>
///   <item>create a <see cref="GrpcAgentSession"/> and register it;</item>
///   <item>close the displaced session returned by the registry (takeover);</item>
///   <item>read Heartbeat/ApplyResult/Health until the stream ends and update state via <see cref="TouchAgentAsync"/> and <see cref="UpdateDeploymentAsync"/>;</item>
///   <item>unregister the session and mark the agent offline in <c>finally</c>.</item>
/// </list>
/// </remarks>
public sealed class AgentControlService(
    IAgentSessionRegistry registry,
    IAgentRepository agents,
    IDeploymentRepository deployments,
    IAuditLog audit,
    ControlPlaneOptions options,
    ILogger<AgentControlService> logger) : AgentControl.AgentControlBase
{
    /// <inheritdoc />
    public override async Task Session(
        IAsyncStreamReader<AgentMessage> requestStream,
        IServerStreamWriter<ControlMessage> responseStream,
        ServerCallContext context)
    {
        var callCancellation = context.CancellationToken;

        if (!await requestStream.MoveNext(callCancellation))
        {
            throw new RpcException(new Status(
                StatusCode.InvalidArgument,
                "The agent must start the session with a Hello message."));
        }

        var firstMessage = requestStream.Current;
        var hello = firstMessage.PayloadCase == AgentMessage.PayloadOneofCase.Hello
            ? firstMessage.Hello
            : null;

        if (!TryParseHello(hello, out var agentId))
        {
            throw new RpcException(new Status(
                StatusCode.InvalidArgument,
                "The first message must be a Hello with a valid agent_id GUID."));
        }

        EnsureCertificateMatches(context, agentId);
        await PersistHelloAsync(hello!, agentId, callCancellation);

        var session = new GrpcAgentSession(agentId, responseStream, context);
        var displaced = registry.Register(agentId, session);
        logger.LogInformation(
            "agent session opened: agent={AgentId} version={AgentVersion} takeover={Takeover}",
            agentId,
            hello!.Version,
            displaced is not null);

        try
        {
            if (displaced is not null)
            {
                await displaced.CloseAsync(CancellationToken.None);
            }

            while (await requestStream.MoveNext(session.Token))
            {
                await DispatchAsync(agentId, requestStream.Current, session.Token);
            }
        }
        catch (OperationCanceledException) when (
            session.Token.IsCancellationRequested || callCancellation.IsCancellationRequested)
        {
            // The agent disconnected or a newer session took over: normal end.
        }
        catch (RpcException ex) when (ex.StatusCode == StatusCode.Cancelled)
        {
            // gRPC reports a cancelled server stream as RpcException(Cancelled).
        }
        finally
        {
            registry.Unregister(agentId, session);
            await session.CloseAsync(CancellationToken.None);
            await MarkOfflineAsync(agentId);
            logger.LogInformation("agent session closed: agent={AgentId}", agentId);
        }
    }

    /// <summary>Applies one agent message to the persisted control-plane state.</summary>
    /// <param name="agentId">Agent that sent the message.</param>
    /// <param name="message">Message received from the stream.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private async Task DispatchAsync(
        Guid agentId,
        AgentMessage message,
        CancellationToken cancellationToken)
    {
        switch (message.PayloadCase)
        {
            case AgentMessage.PayloadOneofCase.Heartbeat:
                await TouchAgentAsync(
                    agentId,
                    NullIfEmpty(message.Heartbeat.ActiveReleaseId),
                    NullIfEmpty(message.Heartbeat.ActiveConfigSha256),
                    cancellationToken);
                break;

            case AgentMessage.PayloadOneofCase.ApplyResult:
                await UpdateDeploymentAsync(agentId, message.ApplyResult, cancellationToken);
                break;

            case AgentMessage.PayloadOneofCase.Health:
                await TouchAgentAsync(
                    agentId,
                    NullIfEmpty(message.Health.ActiveReleaseId),
                    activeConfigSha256: null,
                    cancellationToken);
                break;

            case AgentMessage.PayloadOneofCase.Hello:
                logger.LogWarning(
                    "agent {AgentId} sent Hello after the first message; ignoring",
                    agentId);
                break;

            case AgentMessage.PayloadOneofCase.None:
            default:
                break;
        }
    }

    /// <summary>
    /// Enforces the mTLS identity when the control plane terminates TLS and
    /// requires client certificates.
    /// </summary>
    /// <param name="context">Current gRPC call context.</param>
    /// <param name="agentId">Agent identifier reported in the Hello.</param>
    /// <exception cref="RpcException">
    /// No client certificate is present, or its CN names another agent.
    /// </exception>
    private void EnsureCertificateMatches(ServerCallContext context, Guid agentId)
    {
        if (options.GrpcTls.CertificatePath is null || !options.GrpcTls.RequireClientCertificate)
        {
            return;
        }

        if (!TryGetClientAgentId(context, out var certificateAgentId))
        {
            throw new RpcException(new Status(
                StatusCode.Unauthenticated,
                "A client certificate with CN=wasmgate-agent:<guid> is required."));
        }

        if (certificateAgentId != agentId)
        {
            throw new RpcException(new Status(
                StatusCode.PermissionDenied,
                "The client certificate does not match the Hello agent_id."));
        }
    }

    /// <summary>
    /// Marks an agent offline once its last session has ended. A newer session
    /// that registered during the handover keeps the agent online.
    /// </summary>
    /// <param name="agentId">Agent identifier.</param>
    private async Task MarkOfflineAsync(Guid agentId)
    {
        if (registry.TryGet(agentId, out _))
        {
            return;
        }

        try
        {
            var agent = await agents.FindAsync(agentId, CancellationToken.None);
            if (agent is null || agent.Status == AgentStatus.Offline)
            {
                return;
            }

            agent.Status = AgentStatus.Offline;
            await agents.UpsertAsync(agent, CancellationToken.None);
        }
        catch (Exception ex)
        {
            logger.LogWarning(ex, "failed to mark agent {AgentId} offline", agentId);
        }
    }

    /// <summary>Validates the first agent message and extracts its identifier.</summary>
    /// <param name="hello">Hello payload from the first message, or null.</param>
    /// <param name="agentId">Parsed agent identifier.</param>
    /// <returns>True when the payload is a Hello with a valid GUID agent ID.</returns>
    private static bool TryParseHello(Hello? hello, out Guid agentId)
    {
        agentId = Guid.Empty;
        return hello is not null &&
               !string.IsNullOrWhiteSpace(hello.AgentId) &&
               Guid.TryParse(hello.AgentId, out agentId);
    }

    /// <summary>Extracts the agent identity from the client certificate subject CN.</summary>
    /// <param name="context">Current gRPC call context.</param>
    /// <param name="agentId">Agent identifier encoded in the certificate.</param>
    /// <returns>True when a certificate with a <c>wasmgate-agent:&lt;guid&gt;</c> CN is present.</returns>
    private static bool TryGetClientAgentId(ServerCallContext context, out Guid agentId)
    {
        agentId = Guid.Empty;
        var certificate = context.GetHttpContext()?.Connection.ClientCertificate;
        if (certificate is null)
        {
            return false;
        }

        var commonName = certificate.GetNameInfo(X509NameType.SimpleName, forIssuer: false);
        return AgentNaming.TryParseCertificateCommonName(commonName, out agentId);
    }

    /// <summary>Persists the static and reported fields from a Hello message.</summary>
    /// <param name="hello">Hello payload sent by the agent.</param>
    /// <param name="agentId">Agent identifier from the Hello.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// A registered name is authoritative: when the agent was pre-created (or
    /// enrolled with a name), a different value in Hello only produces a
    /// warning. Labels are merged, so labels configured by an operator survive
    /// a reconnect.
    /// </remarks>
    /// <exception cref="RpcException">
    /// The reported name is invalid or belongs to another agent.
    /// </exception>
    private async Task PersistHelloAsync(Hello hello, Guid agentId, CancellationToken cancellationToken)
    {
        var existing = await agents.FindAsync(agentId, cancellationToken);
        var now = DateTimeOffset.UtcNow;
        var name = existing?.Name;

        if (string.IsNullOrWhiteSpace(name))
        {
            name = string.IsNullOrWhiteSpace(hello.Name)
                ? agentId.ToString("D")
                : hello.Name.Trim();

            if (!AgentNaming.IsValid(name))
            {
                throw new RpcException(new Status(
                    StatusCode.InvalidArgument,
                    "Hello.Name is not a valid agent name."));
            }

            var owner = await agents.FindByNameAsync(name, cancellationToken);
            if (owner is not null && owner.Id != agentId)
            {
                throw new RpcException(new Status(
                    StatusCode.AlreadyExists,
                    $"Agent name '{name}' is already registered."));
            }
        }
        else if (!string.IsNullOrWhiteSpace(hello.Name) &&
                 !string.Equals(hello.Name.Trim(), name, StringComparison.Ordinal))
        {
            logger.LogWarning(
                "agent {AgentId} reported name {ReportedName}, keeping registered name {RegisteredName}",
                agentId,
                hello.Name,
                name);
        }

        var labels = existing is null
            ? new Dictionary<string, string>(StringComparer.Ordinal)
            : existing.Labels.ToDictionary(pair => pair.Key, pair => pair.Value, StringComparer.Ordinal);
        foreach (var pair in hello.Labels)
        {
            labels[pair.Key] = pair.Value;
        }

        var agent = new Agent
        {
            Id = agentId,
            Name = name,
            Version = string.IsNullOrWhiteSpace(hello.Version) ? existing?.Version ?? "0" : hello.Version,
            AbiVersions = hello.AbiVersions.Count > 0
                ? hello.AbiVersions.ToList()
                : existing?.AbiVersions.ToList() ?? [],
            Labels = labels,
            Status = AgentStatus.Online,
            RegisteredAt = existing?.RegisteredAt ?? now,
            LastSeenAt = now,
            ActiveReleaseId = NullIfEmpty(hello.ActiveReleaseId) ?? existing?.ActiveReleaseId,
            ActiveConfigSha256 = NullIfEmpty(hello.ActiveConfigSha256) ?? existing?.ActiveConfigSha256,
        };

        await agents.UpsertAsync(agent, cancellationToken);
    }

    /// <summary>Updates liveness and active-release fields for an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="activeReleaseId">Release reported by the agent, if any.</param>
    /// <param name="activeConfigSha256">Configuration hash reported by the agent, if any.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// A null value keeps the stored value; an empty string clears it. This is
    /// the documented <see cref="IAgentRepository.TouchAsync"/> contract and
    /// lets a Health message omit the configuration hash without erasing it.
    /// </remarks>
    private Task TouchAgentAsync(
        Guid agentId,
        string? activeReleaseId,
        string? activeConfigSha256,
        CancellationToken cancellationToken) =>
        agents.TouchAsync(
            agentId,
            DateTimeOffset.UtcNow,
            activeReleaseId,
            activeConfigSha256,
            cancellationToken);

    /// <summary>Updates deployment state from an agent apply result.</summary>
    /// <param name="agentId">Agent that reported the result.</param>
    /// <param name="result">Apply result sent by the agent.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Only the first result for a running deployment wins. There is no health
    /// gate yet, so the deployment is marked succeeded or failed as soon as one
    /// agent reports a terminal result.
    /// </remarks>
    private async Task UpdateDeploymentAsync(
        Guid agentId,
        ApplyResult result,
        CancellationToken cancellationToken)
    {
        if (!Guid.TryParse(result.DeploymentId, out var deploymentId))
        {
            return;
        }

        var deployment = await deployments.FindAsync(deploymentId, cancellationToken);
        if (deployment is null || deployment.Status != DeploymentStatus.Running)
        {
            return;
        }

        var now = DateTimeOffset.UtcNow;
        deployment.Status = result.Ok ? DeploymentStatus.Succeeded : DeploymentStatus.Failed;
        deployment.CompletedAt = now;

        foreach (var wave in deployment.Waves.Where(w => w.Status != DeploymentWaveStatus.Skipped))
        {
            wave.Status = result.Ok ? DeploymentWaveStatus.Succeeded : DeploymentWaveStatus.Failed;
            wave.CompletedAt = now;
        }

        await deployments.UpdateAsync(deployment, cancellationToken);
        await audit.AppendAsync(
            new AuditEvent
            {
                Actor = AgentNaming.CertificateCommonName(agentId),
                Action = "deployment.apply",
                Target = deploymentId.ToString("D"),
            },
            cancellationToken);

        if (result.Ok)
        {
            await TouchAgentAsync(agentId, NullIfEmpty(result.ReleaseId), activeConfigSha256: null, cancellationToken);
        }
    }

    /// <summary>Converts an empty protobuf string to null.</summary>
    /// <param name="value">Value reported by the agent.</param>
    /// <returns>Null for null or empty input; the original value otherwise.</returns>
    private static string? NullIfEmpty(string? value) =>
        string.IsNullOrEmpty(value) ? null : value;
}
