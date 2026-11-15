using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>
/// Application service that provisions agents, issues enrollment tokens and
/// performs enrollment. This is the automation surface used by the HTTP API,
/// the CLI and CI.
/// </summary>
public interface IAgentProvisioningService
{
    /// <summary>Creates one agent and returns a one-time enrollment token for it.</summary>
    /// <param name="name">Unique agent name.</param>
    /// <param name="labels">Deployment labels used by selectors.</param>
    /// <param name="actor">Identity of the principal creating the agent.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task<AgentProvisioning> CreateAgentAsync(
        string name,
        IReadOnlyDictionary<string, string> labels,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Creates a registration token that many agents can share.</summary>
    /// <param name="namePrefix">
    /// Optional prefix that every enrolling agent name must start with. The
    /// comparison is ordinal and case-sensitive. Use null or an empty string to
    /// allow any valid agent name.
    /// </param>
    /// <param name="maxUses">Maximum number of enrollments.</param>
    /// <param name="ttl">How long the token stays valid.</param>
    /// <param name="actor">Identity of the principal creating the token.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The raw token, its use limit and expiry.</returns>
    Task<RegistrationToken> CreateRegistrationTokenAsync(
        string? namePrefix,
        int maxUses,
        TimeSpan ttl,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Enrolls an agent and returns its client certificate.</summary>
    /// <param name="agentId">Agent identifier claimed by the data plane.</param>
    /// <param name="name">
    /// Optional operator-visible name. It is required when the agent was not
    /// pre-created; for pre-created agents it must match the registered name or
    /// be omitted.
    /// </param>
    /// <param name="token">Raw one-time or registration token.</param>
    /// <param name="csrPem">PEM certificate signing request.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The issued certificate, the agent CA and its expiry.</returns>
    Task<EnrollmentResult> EnrollAsync(
        string agentId,
        string? name,
        string token,
        string csrPem,
        CancellationToken cancellationToken);
}
