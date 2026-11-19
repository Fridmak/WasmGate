using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Grpc;

/// <summary>
/// Periodically re-checks the certificate of a live control session.
/// </summary>
/// <remarks>
/// The read loop blocks until the agent sends a message, so a revoked agent that
/// stops sending heartbeats would otherwise keep its stream open and continue to
/// receive server-to-agent commands. This backstop closes such a session even
/// when the revoke path could not find it in the registry. A failed check is
/// treated as a revocation and closes the session.
/// </remarks>
internal sealed class CertificateSessionMonitor(
    IAgentCertificateRepository certificates,
    IAgentOperationLock agentOperations,
    ControlPlaneOptions options,
    ILogger logger)
{
    /// <summary>Runs until the session ends, the certificate is revoked or the check fails.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="thumbprint">Certificate fingerprint to check.</param>
    /// <param name="session">Session to close when the certificate is no longer active.</param>
    /// <param name="cancellationToken">Cancels the monitor when the session ends.</param>
    public async Task MonitorAsync(
        Guid agentId,
        string thumbprint,
        IAgentSession session,
        CancellationToken cancellationToken)
    {
        var interval = options.CertificateRecheckInterval;
        if (interval <= TimeSpan.Zero)
        {
            interval = TimeSpan.FromSeconds(60);
        }

        try
        {
            using var timer = new PeriodicTimer(interval);
            while (await timer.WaitForNextTickAsync(cancellationToken))
            {
                bool active;
                try
                {
                    await using var agentLease = await agentOperations.AcquireAsync(
                        agentId,
                        cancellationToken);
                    active = await certificates.IsActiveAsync(
                            agentId,
                            thumbprint,
                            cancellationToken);
                }
                catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
                {
                    return;
                }
                catch (Exception ex)
                {
                    logger.LogWarning(
                        ex,
                        "certificate re-check failed; closing session: agent={AgentId}",
                        agentId);
                    await CloseAsync(session, agentId);
                    return;
                }

                if (!active)
                {
                    logger.LogWarning(
                        "closing agent session after certificate revocation: agent={AgentId}",
                        agentId);
                    await CloseAsync(session, agentId);
                    return;
                }
            }
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            // The session ended: normal monitor shutdown.
        }
        catch (Exception ex)
        {
            // Fail closed: an unexpected exception, including an
            // OperationCanceledException not caused by the session token, must
            // not silently disable the revocation backstop.
            logger.LogWarning(
                ex,
                "certificate monitor failed; closing session: agent={AgentId}",
                agentId);
            await CloseAsync(session, agentId);
        }
    }

    /// <summary>Closes the session without letting a transport error escape the monitor.</summary>
    /// <param name="session">Session to close.</param>
    /// <param name="agentId">Agent identifier for the log entry.</param>
    private async Task CloseAsync(IAgentSession session, Guid agentId)
    {
        try
        {
            await session.CloseAsync(CancellationToken.None);
        }
        catch (Exception ex)
        {
            logger.LogWarning(
                ex,
                "failed to close session after certificate re-check: agent={AgentId}",
                agentId);
        }
    }
}
