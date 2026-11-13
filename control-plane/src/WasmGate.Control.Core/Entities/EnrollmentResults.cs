namespace WasmGate.Control.Core.Entities;

/// <summary>Result of issuing one certificate from a CSR.</summary>
/// <param name="CertificatePem">PEM certificate issued by the CA.</param>
/// <param name="SerialNumber">Hex serial number.</param>
/// <param name="Thumbprint">SHA-256 fingerprint of the certificate.</param>
/// <param name="NotBefore">Start of the validity period.</param>
/// <param name="NotAfter">End of the validity period.</param>
public sealed record IssuedCertificate(
    string CertificatePem,
    string SerialNumber,
    string Thumbprint,
    DateTimeOffset NotBefore,
    DateTimeOffset NotAfter);

/// <summary>Result of provisioning one agent: its identifier and one-time token.</summary>
/// <param name="AgentId">Identifier of the created agent.</param>
/// <param name="Token">Raw enrollment token, shown to the operator once.</param>
public sealed record AgentProvisioning(Guid AgentId, string Token);

/// <summary>A registration token that many agents can share during a rollout.</summary>
/// <param name="Token">Raw registration token.</param>
/// <param name="MaxUses">How many enrollments are allowed.</param>
/// <param name="ExpiresAt">When the token stops working.</param>
public sealed record RegistrationToken(string Token, int MaxUses, DateTimeOffset ExpiresAt);

/// <summary>Result of a successful enrollment.</summary>
/// <param name="CertificatePem">Issued client certificate.</param>
/// <param name="CaPem">Agent CA certificate the data plane must trust.</param>
/// <param name="NotAfter">Certificate expiry.</param>
public sealed record EnrollmentResult(string CertificatePem, string CaPem, DateTimeOffset NotAfter);
