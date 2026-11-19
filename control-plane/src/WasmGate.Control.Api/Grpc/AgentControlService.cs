using System.Security.Cryptography.X509Certificates;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Grpc;

/// <summary>
/// Bidirectional control stream for data-plane agents.
/// </summary>
/// <remarks>
/// <para>Flow:</para>
/// <list type="number">
///   <item>read the first message and require a <c>Hello</c>;</item>
///   <item>validate the agent ID with <see cref="TryParseHello"/>;</item>
///   <item>when TLS with client certificates is enabled, compare the certificate identity with the Hello using <c>AgentCertificateIdentity.TryGetAgentId</c>;</item>
///   <item>take the per-agent lifecycle lock, then persist the Hello fields with <see cref="PersistHelloAsync"/> and register a <see cref="GrpcAgentSession"/>;</item>
///   <item>close the displaced session returned by the registry (takeover) after releasing the lock;</item>
///   <item>read Heartbeat/ApplyResult/Health until the stream ends and update state via <see cref="TouchAgentAsync"/> and <see cref="UpdateDeploymentAsync"/>;</item>
///   <item>unregister the session and mark the agent offline in <c>finally</c>.</item>
/// </list>
/// </remarks>
public sealed class AgentControlService(
    IAgentSessionRegistry registry,
    IAgentRepository agents,
    IAgentCertificateRepository certificates,
    IDeploymentRepository deployments,
    IAuditLog audit,
    IAgentOperationLock agentOperations,
    ControlPlaneOptions options,
    ILogger<AgentControlService> logger) : AgentControl.AgentControlBase
{
    private readonly CertificateSessionMonitor _sessionMonitor =
        new(certificates, registry, agentOperations, options, logger);

    /// <summary>
    /// Runs one agent control session until the stream ends.
    /// </summary>
    /// <param name="requestStream">Messages sent by the agent.</param>
    /// <param name="responseStream">Stream used to push commands to the agent.</param>
    /// <param name="context">Context of the gRPC call.</param>
    /// <exception cref="RpcException">
    /// The first message is not a valid <c>Hello</c>, or the client certificate
    /// does not match the agent identifier.
    /// </exception>
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

        var session = new GrpcAgentSession(agentId, responseStream, context);
        IAgentSession? displaced;
        string? sessionCertificateThumbprint = null;
        try
        {
            await using var agentLease = await agentOperations.AcquireAsync(
                agentId,
                callCancellation);
            var certificateThumbprint = await EnsureCertificateMatchesAsync(
                context,
                agentId,
                callCancellation);
            await PersistHelloAsync(hello!, agentId, callCancellation);
            displaced = registry.Register(agentId, session);

            // Keep the thumbprint for the monitor after the lifecycle lock is
            // released. The local is copied into the monitor scope below.
            sessionCertificateThumbprint = certificateThumbprint;
        }
        catch
        {
            await session.CloseAsync(CancellationToken.None);
            throw;
        }

        // Logging is outside the registration critical section: a failing log
        // sink must not leave a registered session behind.
        try
        {
            logger.LogInformation(
                "agent session registered: agent={AgentId} version={AgentVersion} takeover={Takeover}",
                agentId,
                hello!.Version,
                displaced is not null);
        }
        catch (Exception)
        {
            // Logging is best-effort and must not break a live session.
        }

        using var monitorCancellation =
            CancellationTokenSource.CreateLinkedTokenSource(session.Token);
        var monitorTask = sessionCertificateThumbprint is null
            ? Task.CompletedTask
            : _sessionMonitor.MonitorAsync(
                agentId,
                sessionCertificateThumbprint,
                session,
                monitorCancellation.Token);

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
            monitorCancellation.Cancel();
            try
            {
                await monitorTask;
            }
            catch (OperationCanceledException) when (monitorCancellation.IsCancellationRequested)
            {
                // Normal shutdown of the periodic certificate monitor.
            }
            catch (Exception ex)
            {
                logger.LogWarning(
                    ex,
                    "certificate monitor failed: agent={AgentId}",
                    agentId);
            }

            await using (await agentOperations.AcquireAsync(agentId, CancellationToken.None))
            {
                registry.Unregister(agentId, session);
                // Keep the offline update under the lock so it cannot overwrite
                // a status written by a concurrent new session.
                await MarkOfflineAsync(agentId);
            }
            await session.CloseAsync(CancellationToken.None);
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
    /// Enforces the mTLS identity and the revocation state when the control plane
    /// terminates TLS.
    /// </summary>
    /// <param name="context">Current gRPC call context.</param>
    /// <param name="agentId">Agent identifier reported in the Hello.</param>
    /// <param name="cancellationToken">Cancels the certificate lookup.</param>
    /// <remarks>
    /// The TLS layer only requests the certificate, so this is the enforcement
    /// point: the certificate must be present when required, its CN must name the
    /// Hello agent, and it must still be active. The revocation lookup exists
    /// because the TLS stack cannot consult a CRL.
    /// </remarks>
    /// <exception cref="RpcException">
    /// The certificate is missing, names another agent, or was revoked or expired.
    /// </exception>
    private Task<string?> EnsureCertificateMatchesAsync(
        ServerCallContext context,
        Guid agentId,
        CancellationToken cancellationToken)
    {
        if (options.GrpcTls.CertificatePath is null)
        {
            return Task.FromResult<string?>(null);
        }

        return ValidateCertificateAsync(
            AgentCertificateIdentity.GetCertificate(context),
            agentId,
            cancellationToken);
    }

    /// <summary>
    /// Validates a presented certificate against the endpoint policy, the agent
    /// identity and the certificate repository.
    /// </summary>
    /// <param name="certificate">Certificate presented by the agent, or null.</param>
    /// <param name="agentId">Agent identifier reported in the Hello.</param>
    /// <param name="cancellationToken">Cancels the certificate lookup.</param>
    /// <returns>The certificate thumbprint, or null when mTLS is not required.</returns>
    /// <remarks>
    /// This is the enforcement point for session start. Revoke, rotation,
    /// deletion and the certificate monitor remove a live session under the
    /// same per-agent lock, so a registration cannot outlive a committed
    /// revocation.
    /// </remarks>
    /// <exception cref="RpcException">
    /// The certificate is missing, names another agent, or was revoked or expired.
    /// </exception>
    internal async Task<string?> ValidateCertificateAsync(
        X509Certificate2? certificate,
        Guid agentId,
        CancellationToken cancellationToken)
    {
        if (options.GrpcTls.CertificatePath is null)
        {
            return null;
        }

        if (certificate is null)
        {
            if (!options.GrpcTls.RequireClientCertificate)
            {
                return null;
            }

            throw new RpcException(new Status(
                StatusCode.Unauthenticated,
                "A client certificate with CN=wasmgate-agent:<guid> is required."));
        }

        if (!AgentNaming.TryParseCertificateCommonName(
                certificate.GetNameInfo(X509NameType.SimpleName, forIssuer: false),
                out var certificateAgentId) ||
            certificateAgentId != agentId)
        {
            throw new RpcException(new Status(
                StatusCode.PermissionDenied,
                "The client certificate does not match the Hello agent_id."));
        }

        var thumbprint = AgentCertificateIdentity.Thumbprint(certificate)!;
        if (!await certificates.IsActiveAsync(agentId, thumbprint, cancellationToken))
        {
            throw new RpcException(new Status(
                StatusCode.Unauthenticated,
                "The client certificate was revoked or has expired."));
        }

        return thumbprint;
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
            if (!await agents.UpsertAsync(agent, CancellationToken.None))
            {
                logger.LogInformation(
                    "agent {AgentId} was deleted; skipped offline update",
                    agentId);
            }
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
    /// The reported name is invalid or belongs to another agent, or the agent was
    /// deleted while reconnecting.
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
            // Operator labels win: an agent must not be able to move itself into
            // another deployment wave by reporting a different value.
            labels.TryAdd(pair.Key, pair.Value);
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

        if (!await agents.UpsertAsync(agent, cancellationToken))
        {
            logger.LogWarning(
                "agent {AgentId} reconnected after deletion; rejecting the session",
                agentId);
            throw new RpcException(new Status(
                StatusCode.PermissionDenied,
                "Agent was deleted and cannot reconnect."));
        }
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

        var agent = await agents.FindAsync(agentId, cancellationToken);
        if (agent is null ||
            !string.Equals(agent.Environment, deployment.Environment, StringComparison.OrdinalIgnoreCase))
        {
            logger.LogWarning(
                "ignoring apply result from agent {AgentId} for deployment {DeploymentId}: environment mismatch",
                agentId,
                deploymentId);
            return;
        }

        if (!string.IsNullOrWhiteSpace(result.ReleaseId) &&
            !string.Equals(result.ReleaseId, deployment.ReleaseId.ToString("D"), StringComparison.OrdinalIgnoreCase))
        {
            logger.LogWarning(
                "ignoring apply result from agent {AgentId} for deployment {DeploymentId}: release mismatch",
                agentId,
                deploymentId);
            return;
        }

        var targeted = deployment.Waves.Any(wave =>
        {
            try
            {
                return WaveSelector.Parse(wave.Selector).Matches(agent);
            }
            catch (InvalidOperationException)
            {
                return false;
            }
        });
        if (!targeted)
        {
            logger.LogWarning(
                "ignoring apply result from agent {AgentId} for deployment {DeploymentId}: agent is not a target",
                agentId,
                deploymentId);
            return;
        }

        var now = DateTimeOffset.UtcNow;
        deployment.Status = result.Ok ? DeploymentStatus.Succeeded : DeploymentStatus.Failed;
        deployment.CompletedAt = now;
        deployment.Message = result.Ok
            ? null
            : await DescribeApplyFailureAsync(agentId, result, cancellationToken);

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

    /// <summary>Maximum length of a stored deployment message.</summary>
    private const int MaxDeploymentMessageLength = 4000;

    /// <summary>Builds a human-readable apply failure from an agent result.</summary>
    /// <param name="agentId">Agent that reported the failure.</param>
    /// <param name="result">Apply result sent by the agent.</param>
    /// <param name="cancellationToken">Cancels the agent lookup.</param>
    /// <returns>A message naming the agent, the error and any listener errors.</returns>
    private async Task<string> DescribeApplyFailureAsync(
        Guid agentId,
        ApplyResult result,
        CancellationToken cancellationToken)
    {
        var agent = await agents.FindAsync(agentId, cancellationToken);
        var reporter = agent?.Name ?? AgentNaming.CertificateCommonName(agentId);

        var parts = new List<string>();
        if (!string.IsNullOrWhiteSpace(result.Error))
        {
            parts.Add(result.Error.Trim());
        }

        foreach (var listener in result.Listeners.Where(
                     listener => !string.IsNullOrWhiteSpace(listener.Error)))
        {
            var name = string.IsNullOrWhiteSpace(listener.Name)
                ? listener.Address
                : listener.Name;
            parts.Add($"{name}: {listener.Error.Trim()}");
        }

        var detail = parts.Count == 0
            ? "agent reported failure without a message"
            : string.Join("; ", parts);
        var message = $"Agent '{reporter}' reported: {detail}";

        return message.Length <= MaxDeploymentMessageLength
            ? message
            : message[..MaxDeploymentMessageLength];
    }
}
