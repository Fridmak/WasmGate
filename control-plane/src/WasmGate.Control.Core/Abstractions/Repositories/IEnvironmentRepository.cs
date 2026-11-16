namespace WasmGate.Control.Core.Abstractions.Repositories;

/// <summary>
/// Manages the environments known to the control plane.
/// </summary>
/// <remarks>
/// <para>
/// Environments are namespaces for configurations, releases, deployments and
/// agents. They live in their own table so an environment can be created before
/// it has content; <c>prod</c> and <c>staging</c> are seeded by the schema.
/// </para>
/// <para>
/// Deployment isolation is enforced on agents, not on this catalog: a deployment
/// only targets agents whose <c>Environment</c> matches. The catalog exists for
/// the picker and for validation.
/// </para>
/// </remarks>
public interface IEnvironmentRepository
{
    /// <summary>Returns every known environment sorted ordinally.</summary>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Distinct environment names, including names referenced by existing data.</returns>
    Task<IReadOnlyList<string>> ListAsync(CancellationToken cancellationToken);

    /// <summary>Creates an environment if it does not exist yet.</summary>
    /// <param name="name">Normalised environment name.</param>
    /// <param name="actor">Identity of the principal creating it.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>True when the environment was created, false when it already existed.</returns>
    /// <remarks>Idempotent: creating an existing environment is not an error.</remarks>
    Task<bool> EnsureAsync(string name, string actor, CancellationToken cancellationToken);

    /// <summary>Deletes an environment and everything scoped to it.</summary>
    /// <param name="name">Normalised environment name.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <remarks>
    /// Allowed only when no agent belongs to the environment and no deployment is
    /// still running; agents are not history and must be moved explicitly.
    /// Configurations, releases, deployments and enrollment tokens of the
    /// environment are deleted in one transaction. The default environment
    /// <c>prod</c> cannot be deleted.
    /// </remarks>
    /// <exception cref="KeyNotFoundException">The environment does not exist.</exception>
    /// <exception cref="InvalidOperationException">
    /// The environment is the default one, still has agents, or has a running
    /// deployment.
    /// </exception>
    Task DeleteAsync(string name, CancellationToken cancellationToken);
}
