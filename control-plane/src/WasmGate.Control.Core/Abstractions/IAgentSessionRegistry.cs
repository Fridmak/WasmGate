using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>Live agent connections. Implemented in memory for now.</summary>
public interface IAgentSessionRegistry
{
    /// <summary>
    /// Number of agents that currently have a registered session. A takeover
    /// replaces the value but does not increase the count.
    /// </summary>
    int Count { get; }

    /// <summary>
    /// Registers a connected agent session and returns the session it displaced.
    /// </summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="session">New session that can receive commands.</param>
    /// <returns>
    /// The previous session when the agent reconnected, or null for the first
    /// registration or when the same session instance was registered again. The
    /// caller owns the returned session and must close it via
    /// <see cref="IAgentSession.CloseAsync"/> to implement takeover semantics.
    /// </returns>
    IAgentSession? Register(Guid agentId, IAgentSession session);

    /// <summary>Removes a session when the agent disconnects.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="session">Session being removed; ignored when it was already replaced.</param>
    void Unregister(Guid agentId, IAgentSession session);

    /// <summary>Looks up the current session of an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="session">The connected session, or null when the agent is offline.</param>
    /// <returns>True when a session exists.</returns>
    bool TryGet(Guid agentId, [System.Diagnostics.CodeAnalysis.NotNullWhen(true)] out IAgentSession? session);
}

/// <summary>A connected data plane that can receive control commands.</summary>
public interface IAgentSession
{
    /// <summary>Identifier of the connected agent.</summary>
    Guid AgentId { get; }

    /// <summary>Sends one command to the agent.</summary>
    /// <param name="command">Transport-neutral command to send.</param>
    /// <param name="cancellationToken">Cancels the send operation.</param>
    ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken);

    /// <summary>
    /// Requests the session to terminate and cancels its underlying transport.
    /// </summary>
    /// <remarks>
    /// The implementation should cancel the gRPC stream / session token. It must
    /// be idempotent and safe to call after the session has already ended. The
    /// method should not block indefinitely: the caller uses it during takeover.
    /// </remarks>
    ValueTask CloseAsync(CancellationToken cancellationToken);
}
