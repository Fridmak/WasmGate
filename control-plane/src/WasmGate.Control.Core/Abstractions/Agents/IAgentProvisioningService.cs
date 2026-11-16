using WasmGate.Control.Core.Entities.Agents.Enrollment;

namespace WasmGate.Control.Core.Abstractions.Agents;

/// <summary>
/// Provisions agents: creates them, issues enrollment tokens and enrolls data
/// planes. Backs the admin HTTP API and the enrollment gRPC service.
/// </summary>
public interface IAgentProvisioningService
{
    /// <summary>Creates one agent in Pending state with a single-use enrollment token.</summary>
    /// <param name="name">Unique agent name.</param>
    /// <param name="labels">Deployment labels used by wave selectors.</param>
    /// <param name="actor">Identity of the principal creating the agent.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The new agent identifier and its raw one-time token.</returns>
    /// <exception cref="EnrollmentException">
    /// The name is invalid or already registered.
    /// </exception>
    Task<AgentProvisioning> CreateAgentAsync(
        string name,
        IReadOnlyDictionary<string, string> labels,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Creates a registration token that several agents can share.</summary>
    /// <param name="namePrefix">
    /// Optional prefix that every enrolling agent name must start with; the
    /// comparison is ordinal and case-sensitive. Null or empty accepts any name.
    /// </param>
    /// <param name="maxUses">Maximum number of enrollments.</param>
    /// <param name="ttl">How long the token stays valid.</param>
    /// <param name="actor">Identity of the principal creating the token.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// The raw token, its use limit and expiry. Only a SHA-256 hash of the token
    /// is stored, so the value cannot be recovered later.
    /// </returns>
    Task<RegistrationToken> CreateRegistrationTokenAsync(
        string? namePrefix,
        int maxUses,
        TimeSpan ttl,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Enrolls an agent and returns its client certificate.</summary>
    /// <param name="agentId">Agent identifier claimed by the data plane.</param>
    /// <param name="name">
    /// Optional operator-visible name. Required when the agent was not
    /// pre-created; for a pre-created agent it must match the registered name or
    /// be omitted.
    /// </param>
    /// <param name="token">Raw one-time or registration token.</param>
    /// <param name="csrPem">PEM certificate signing request; only RSA keys are accepted.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The issued certificate, the agent CA and its expiry.</returns>
    /// <remarks>
    /// The token is consumed before the certificate is issued, so a rejected CSR
    /// still spends one use. The certificate is recorded for later revocation.
    /// </remarks>
    /// <exception cref="EnrollmentException">
    /// The identifier, token, name scope or CSR is rejected.
    /// </exception>
    Task<EnrollmentResult> EnrollAsync(
        string agentId,
        string? name,
        string token,
        string csrPem,
        CancellationToken cancellationToken);
}
