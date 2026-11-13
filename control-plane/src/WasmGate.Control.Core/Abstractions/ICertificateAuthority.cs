using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Core.Abstractions;

/// <summary>
/// The agent certificate authority. It owns the CA key pair and signs
/// certificate signing requests from data planes.
/// </summary>
public interface ICertificateAuthority
{
    /// <summary>Returns the PEM certificate of the agent CA.</summary>
    /// <param name="cancellationToken">Cancels the operation.</param>
    Task<string> GetCaCertificatePemAsync(CancellationToken cancellationToken);

    /// <summary>Issues a client certificate from a CSR.</summary>
    /// <param name="csrPem">PEM certificate signing request.</param>
    /// <param name="commonName">Expected subject CN, for example wasmgate-agent:&lt;guid&gt;.</param>
    /// <param name="notAfter">Certificate expiry.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The issued certificate and its metadata.</returns>
    Task<IssuedCertificate> IssueAsync(
        string csrPem,
        string commonName,
        DateTimeOffset notAfter,
        CancellationToken cancellationToken);
}
