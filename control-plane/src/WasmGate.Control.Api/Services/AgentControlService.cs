using System.Security.Cryptography.X509Certificates;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Services;

/// <summary>
/// Bidirectional control stream for data-plane agents. This service owns the
/// gRPC adapter only; persistence, deployment orchestration and heartbeat
/// timeouts belong to the application services.
/// </summary>
public sealed class AgentControlService(
    IAgentSessionRegistry registry,
    IAgentRepository agents,
    IDeploymentRepository deployments,
    ControlPlaneOptions options,
    ILogger<AgentControlService> logger) : AgentControl.AgentControlBase
{
    private const string AgentCertificatePrefix = "wasmgate-agent:";

    /// <inheritdoc />
    public override async Task Session(
        IAsyncStreamReader<AgentMessage> requestStream,
        IServerStreamWriter<ControlMessage> responseStream,
        ServerCallContext context)
    {
        if (!await requestStream.MoveNext(context.CancellationToken))
        {
            return;
        }

        var first = requestStream.Current;
        if (first.PayloadCase != AgentMessage.PayloadOneofCase.Hello ||
            !TryParseHello(first.Hello, out var agentId))
        {
            throw new RpcException(new Status(
                StatusCode.InvalidArgument,
                "The first message must be a valid Hello."));
        }

        // The Hello.AgentId is a claim, not an identity. When mTLS is enabled,
        // the certificate must belong to the same agent before the session can
        // replace the current one.
        var requireClientCertificate =
            options.GrpcTls is { IsConfigured: true, RequireClientCertificate: true };

        if (requireClientCertificate)
        {
            if (!TryGetClientAgentId(context, out var certificateAgentId))
            {
                throw new RpcException(new Status(
                    StatusCode.Unauthenticated,
                    "A client certificate with a wasmgate-agent identity is required."));
            }

            if (certificateAgentId != agentId)
            {
                throw new RpcException(new Status(
                    StatusCode.PermissionDenied,
                    "The agent ID does not match the client certificate."));
            }
        }
        else
        {
            logger.LogWarning(
                "client certificate authentication is disabled; accepting unauthenticated agent {AgentId}",
                agentId);
        }

        var session = new GrpcAgentSession(agentId, responseStream, context);
        var previous = registry.Register(agentId, session);

        if (previous is not null)
        {
            try
            {
                using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
                await previous.CloseAsync(timeout.Token);
                logger.LogInformation("agent session replaced: {AgentId}", agentId);
            }
            catch (Exception ex)
            {
                // The new session is already registered. The old one will be
                // collected by the heartbeat reaper if it does not stop.
                logger.LogWarning(ex, "failed to close displaced session: {AgentId}", agentId);
            }
        }

        try
        {
            await foreach (var message in requestStream.ReadAllAsync(session.Token))
            {
                switch (message.PayloadCase)
                {
                    case AgentMessage.PayloadOneofCase.Heartbeat:
                        logger.LogDebug(
                            "agent heartbeat: {AgentId} active={Active}",
                            message.Heartbeat.AgentId,
                            message.Heartbeat.ConnectionsActive);
                        await agents.TouchAsync(
                            agentId,
                            DateTimeOffset.UtcNow,
                            message.Heartbeat.ActiveReleaseId,
                            activeConfigSha256: null,
                            context.CancellationToken);
                        break;

                    case AgentMessage.PayloadOneofCase.ApplyResult:
                        logger.LogInformation(
                            "agent apply result: {AgentId} release={ReleaseId} ok={Ok} error={Error}",
                            agentId,
                            message.ApplyResult.ReleaseId,
                            message.ApplyResult.Ok,
                            message.ApplyResult.Error);
                        await UpdateDeploymentAsync(message.ApplyResult, context.CancellationToken);
                        break;

                    case AgentMessage.PayloadOneofCase.Health:
                        logger.LogDebug(
                            "agent health: {AgentId} release={ReleaseId} active={Active}",
                            agentId,
                            message.Health.ActiveReleaseId,
                            message.Health.ConnectionsActive);
                        break;

                    default:
                        logger.LogDebug(
                            "unexpected agent message: {AgentId} type={PayloadCase}",
                            agentId,
                            message.PayloadCase);
                        break;
                }
            }
        }
        catch (OperationCanceledException) when (session.Token.IsCancellationRequested)
        {
            // Normal disconnect, shutdown or takeover.
        }
        catch (RpcException ex) when (ex.StatusCode == StatusCode.Cancelled)
        {
            // gRPC reports a cancelled client stream as RpcException.
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "agent session failed: {AgentId}", agentId);
            throw new RpcException(new Status(StatusCode.Internal, "Agent session failed."));
        }
        finally
        {
            registry.Unregister(agentId, session);
            logger.LogInformation("agent session closed: {AgentId}", agentId);
        }
    }

    /// <summary>Updates deployment status from an agent apply result.</summary>
    /// <param name="result">Apply result sent by the agent.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private async Task UpdateDeploymentAsync(ApplyResult result, CancellationToken cancellationToken)
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

        deployment.Status = result.Ok ? DeploymentStatus.Succeeded : DeploymentStatus.Failed;
        deployment.CompletedAt = DateTimeOffset.UtcNow;
        foreach (var wave in deployment.Waves)
        {
            wave.Status = result.Ok ? DeploymentWaveStatus.Succeeded : DeploymentWaveStatus.Failed;
            wave.CompletedAt = deployment.CompletedAt;
        }

        await deployments.UpdateAsync(deployment, cancellationToken);
    }

    /// <summary>Validates the first agent message and extracts its identifier.</summary>
    /// <param name="hello">Hello payload, or null when another oneof case arrived.</param>
    /// <param name="agentId">Parsed agent identifier when validation succeeds.</param>
    /// <returns>True when the Hello contains a valid non-empty name and GUID identifier.</returns>
    private static bool TryParseHello(Hello? hello, out Guid agentId)
    {
        agentId = Guid.Empty;

        return hello is not null &&
               !string.IsNullOrWhiteSpace(hello.Name) &&
               !string.IsNullOrWhiteSpace(hello.AgentId) &&
               Guid.TryParse(hello.AgentId, out agentId);
    }

    /// <summary>
    /// Extracts the agent identity from the mTLS client certificate. The
    /// certificate subject CN must use the form
    /// <c>wasmgate-agent:&lt;guid&gt;</c>.
    /// </summary>
    /// <param name="context">Current gRPC call context.</param>
    /// <param name="agentId">Agent identifier encoded in the certificate.</param>
    /// <returns>True when the client certificate carries a valid agent identity.</returns>
    private static bool TryGetClientAgentId(ServerCallContext context, out Guid agentId)
    {
        agentId = Guid.Empty;

        var certificate = context.GetHttpContext().Connection.ClientCertificate;
        if (certificate is null)
        {
            return false;
        }

        var commonName = certificate.GetNameInfo(X509NameType.SimpleName, forIssuer: false);
        return commonName.StartsWith(AgentCertificatePrefix, StringComparison.OrdinalIgnoreCase) &&
               Guid.TryParse(commonName[AgentCertificatePrefix.Length..], out agentId);
    }
}
