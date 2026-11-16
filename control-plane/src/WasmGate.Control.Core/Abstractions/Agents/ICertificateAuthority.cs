using WasmGate.Control.Core.Entities.Agents.Enrollment;

namespace WasmGate.Control.Core.Abstractions.Agents;

/// <summary>
/// The agent certificate authority: owns the CA key pair and signs certificate
/// requests from data planes.
/// </summary>
/// <remarks>
/// The CA files are created on first use when they are missing. Deleting them
/// invalidates every certificate issued so far.
/// </remarks>
public interface ICertificateAuthority
{
    /// <summary>Returns the PEM certificate of the agent CA.</summary>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task<string> GetCaCertificatePemAsync(CancellationToken cancellationToken);

    /// <summary>Issues a client certificate from a CSR.</summary>
    /// <param name="csrPem">PEM certificate signing request.</param>
    /// <param name="commonName">
    /// Subject CN written into the issued certificate, for example
    /// wasmgate-agent:&lt;guid&gt;. The subject of the CSR is not used.
    /// </param>
    /// <param name="notAfter">Certificate expiry; must be in the future.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The issued certificate and its metadata.</returns>
    /// <exception cref="EnrollmentException">The CSR is malformed or not RSA.</exception>
    Task<IssuedCertificate> IssueAsync(
        string csrPem,
        string commonName,
        DateTimeOffset notAfter,
        CancellationToken cancellationToken);
}
