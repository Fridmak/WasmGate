using WasmGate.Control.Core.Entities.Agents;
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
    /// <param name="environment">
    /// Deployment environment the agent belongs to. Null or blank means
    /// <c>prod</c>; the name is normalised and created when it is new.
    /// </param>
    /// <param name="labels">Deployment labels used by wave selectors.</param>
    /// <param name="actor">Identity of the principal creating the agent.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The new agent identifier and its raw one-time token.</returns>
    /// <exception cref="EnrollmentException">
    /// The name or environment is invalid, or the name is already registered.
    /// </exception>
    Task<AgentProvisioning> CreateAgentAsync(
        string name,
        string? environment,
        IReadOnlyDictionary<string, string> labels,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Creates a registration token that several agents can share.</summary>
    /// <param name="namePrefix">
    /// Optional prefix that every enrolling agent name must start with; the
    /// comparison is ordinal and case-sensitive. Null or empty accepts any name.
    /// </param>
    /// <param name="environment">
    /// Environment a newly enrolled agent joins. Null or blank means <c>prod</c>.
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
        string? environment,
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

    /// <summary>Issues a fresh single-use enrollment token for an existing agent.</summary>
    /// <param name="agentId">Registered agent identifier.</param>
    /// <param name="actor">Identity of the principal issuing the token.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The raw token, its single use and its expiry.</returns>
    /// <remarks>
    /// Recovers an agent whose token was lost before the first enrollment. The
    /// token is not tied to the certificate lifecycle; renewal uses
    /// <see cref="RenewAsync"/> instead.
    /// </remarks>
    /// <exception cref="EnrollmentException">The agent is not registered.</exception>
    Task<RegistrationToken> IssueEnrollmentTokenAsync(
        Guid agentId,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Moves a registered agent to another environment.</summary>
    /// <param name="agentId">Registered agent identifier.</param>
    /// <param name="environment">Target environment; created when it is new.</param>
    /// <param name="actor">Identity of the principal performing the move.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Deployments only target agents of their own environment, so a move takes
    /// effect at the next deployment. The release currently applied by the agent
    /// is not changed.
    /// </remarks>
    /// <exception cref="EnrollmentException">
    /// The agent is unknown or the environment name is invalid.
    /// </exception>
    Task SetEnvironmentAsync(
        Guid agentId,
        string environment,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Deletes an agent record and closes its live session.</summary>
    /// <param name="agentId">Registered agent identifier.</param>
    /// <param name="actor">Identity of the principal performing the deletion.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Enrollment tokens and issued certificates are removed with the agent
    /// through foreign-key cascades. The data-plane process is not stopped: if it
    /// is still running, it will be rejected on its next connection because its
    /// certificate no longer exists. Stop the process on the host as well.
    /// </remarks>
    /// <exception cref="KeyNotFoundException">The agent is not registered.</exception>
    Task DeleteAgentAsync(
        Guid agentId,
        string actor,
        CancellationToken cancellationToken);

    /// <summary>Renews the client certificate of an already authenticated agent.</summary>
    /// <param name="agentId">Agent identifier taken from the client certificate.</param>
    /// <param name="csrPem">PEM certificate signing request with a new key.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The new certificate, the agent CA and its expiry.</returns>
    /// <remarks>
    /// Authorisation comes from the existing mTLS identity, not from a token: the
    /// gRPC layer verifies that the certificate CN names the requested agent.
    /// The expired certificate is replaced, so renewal must happen before it
    /// expires; a data plane does this automatically.
    /// </remarks>
    /// <exception cref="EnrollmentException">
    /// The agent is unknown or the CSR is invalid.
    /// </exception>
    Task<EnrollmentResult> RenewAsync(
        Guid agentId,
        string csrPem,
        CancellationToken cancellationToken);
}
