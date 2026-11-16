using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;

namespace WasmGate.Control.Api.Infrastructure.Networking;

/// <summary>
/// Validates the client certificates that agents present to the gRPC endpoint.
/// </summary>
internal static class ClientCertificateValidator
{
    /// <summary>OID of the client-authentication EKU (1.3.6.1.5.5.7.3.2).</summary>
    private const string ClientAuthOid = "1.3.6.1.5.5.7.3.2";

    /// <summary>Loads the trusted agent CA certificates from a PEM bundle.</summary>
    /// <param name="path">Path to a PEM file with one or more CA certificates.</param>
    /// <returns>A non-empty collection of CA certificates.</returns>
    /// <exception cref="ArgumentException">
    /// The path is missing, the file cannot be read, or it contains no certificate.
    /// </exception>
    public static X509Certificate2Collection LoadBundle(string? path)
    {
        if (string.IsNullOrWhiteSpace(path))
        {
            throw new ArgumentException(
                "client certificate authentication requires a client CA bundle",
                nameof(path));
        }

        var bundle = new X509Certificate2Collection();
        try
        {
            bundle.ImportFromPem(File.ReadAllText(path));
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or CryptographicException)
        {
            throw new ArgumentException($"cannot load client CA bundle: {ex.Message}", nameof(path), ex);
        }

        if (bundle.Count == 0)
        {
            throw new ArgumentException("client CA bundle contains no certificates", nameof(path));
        }

        return bundle;
    }

    /// <summary>Validates an agent certificate against the trusted CA bundle.</summary>
    /// <param name="certificate">Certificate presented by the agent.</param>
    /// <param name="trustedCas">CA certificates trusted for agent identities.</param>
    /// <returns>True when the chain builds to one of the trusted CAs and the purpose matches.</returns>
    /// <remarks>
    /// Revocation is not checked, because the control plane does not publish a CRL.
    /// When the certificate carries an EKU, the client-authentication purpose is
    /// required; an absent EKU is accepted, as some private CAs omit it.
    /// </remarks>
    public static bool Validate(X509Certificate2? certificate, X509Certificate2Collection trustedCas)
    {
        if (certificate is null)
        {
            return false;
        }

        using var chain = new X509Chain();
        chain.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
        chain.ChainPolicy.CustomTrustStore.AddRange(trustedCas);
        chain.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
        chain.ChainPolicy.VerificationFlags = X509VerificationFlags.NoFlag;

        if (!chain.Build(certificate))
        {
            return false;
        }

        var eku = certificate.Extensions
            .OfType<X509EnhancedKeyUsageExtension>()
            .FirstOrDefault();

        return eku is null ||
               eku.EnhancedKeyUsages
                   .Cast<Oid>()
                   .Any(oid => oid.Value == ClientAuthOid);
    }
}
