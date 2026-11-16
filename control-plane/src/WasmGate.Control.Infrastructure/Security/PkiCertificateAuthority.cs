using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Agents.Enrollment;

namespace WasmGate.Control.Infrastructure.Security;

/// <summary>
/// Agent certificate authority backed by a PEM key pair on disk.
/// </summary>
/// <remarks>
/// <para>
/// The CA is an RSA-2048 self-signed certificate valid for ten years, created on
/// first use when both files are missing. The private key is written with
/// owner-only permissions on Unix.
/// </para>
/// <para>
/// Issued certificates are leaf certificates with CN=<c>wasmgate-agent:&lt;guid&gt;</c>,
/// digital-signature key usage and the client-authentication EKU. Only RSA
/// certificate requests are accepted. If exactly one of the two CA files exists,
/// initialization fails instead of silently creating a new CA, because that would
/// invalidate every certificate already issued.
/// </para>
/// </remarks>
public sealed class PkiCertificateAuthority : ICertificateAuthority
{
    private const string CaCommonName = "WasmGate Agent CA";

    private readonly ControlPlaneOptions _options;
    private readonly Lock _gate = new();

    private X509Certificate2? _caCertificate;

    /// <summary>Loaded or generated agent CA certificate.</summary>
    private X509Certificate2 CaCertificate
    {
        get
        {
            lock (_gate)
            {
                _caCertificate ??= LoadOrCreate();
                return _caCertificate;
            }
        }
    }

    /// <summary>Creates the authority and ensures the CA key pair exists.</summary>
    public PkiCertificateAuthority(ControlPlaneOptions options)
    {
        _options = options;
        _ = CaCertificate;
    }

    /// <inheritdoc />
    public Task<string> GetCaCertificatePemAsync(CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        var pem = CaCertificate.ExportCertificatePem();
        return Task.FromResult(pem);
    }

    /// <inheritdoc />
    public Task<IssuedCertificate> IssueAsync(
        string csrPem,
        string commonName,
        DateTimeOffset notAfter,
        CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();

        var notBefore = DateTimeOffset.UtcNow.AddMinutes(-5);
        if (notAfter <= notBefore)
        {
            throw new ArgumentOutOfRangeException(
                nameof(notAfter),
                notAfter,
                "The certificate expiry must be in the future.");
        }

        CertificateRequest csr;
        try
        {
            csr = CertificateRequest.LoadSigningRequestPem(
                csrPem,
                HashAlgorithmName.SHA256,
                CertificateRequestLoadOptions.Default,
                RSASignaturePadding.Pkcs1);
        }
        catch (Exception ex)
        {
            throw new EnrollmentException(EnrollmentFailure.CsrInvalid, $"Invalid CSR: {ex.Message}");
        }

        var rsa = csr.PublicKey.GetRSAPublicKey()
                  ?? throw new EnrollmentException(
                      EnrollmentFailure.CsrInvalid,
                      "Only RSA certificate requests are supported.");

        var subject = new X500DistinguishedName("CN=" + commonName);
        var request = new CertificateRequest(
            subject,
            rsa,
            HashAlgorithmName.SHA256,
            RSASignaturePadding.Pkcs1);

        request.CertificateExtensions.Add(new X509BasicConstraintsExtension(false, false, 0, false));
        request.CertificateExtensions.Add(new X509KeyUsageExtension(X509KeyUsageFlags.DigitalSignature, false));
        request.CertificateExtensions.Add(
            new X509EnhancedKeyUsageExtension(
                new OidCollection { new Oid("1.3.6.1.5.5.7.3.2") },
                critical: false));

        X509Certificate2 issued;
        lock (_gate)
        {
            var ca = CaCertificate;
            issued = request.Create(
                ca,
                notBefore,
                notAfter,
                RandomNumberGenerator.GetBytes(16));
        }

        using (issued)
        {
            var pem = issued.ExportCertificatePem();
            return Task.FromResult(new IssuedCertificate(
                CertificatePem: pem,
                SerialNumber: issued.SerialNumber,
                Thumbprint: Convert.ToHexString(SHA256.HashData(issued.RawData)),
                NotBefore: notBefore,
                NotAfter: notAfter));
        }
    }

    /// <summary>Loads the CA key pair or generates it when the files are missing.</summary>
    /// <returns>A certificate usable for signing agent certificates.</returns>
    /// <exception cref="InvalidOperationException">
    /// Only one of the certificate/key files exists, or the pair cannot be loaded.
    /// </exception>
    private X509Certificate2 LoadOrCreate()
    {
        var certPath = _options.AgentCaCertPath;
        var keyPath = _options.AgentCaKeyPath;

        var hasCertificate = File.Exists(certPath);
        var hasKey = File.Exists(keyPath);
        if (hasCertificate != hasKey)
        {
            throw new InvalidOperationException(
                "The agent CA is incomplete: " +
                $"certificate '{certPath}' exists={hasCertificate}, key '{keyPath}' exists={hasKey}. " +
                "Restore the missing file or remove both files to generate a new CA. " +
                "Generating a new CA invalidates every issued agent certificate.");
        }

        if (hasCertificate)
        {
            try
            {
                using var pem = X509Certificate2.CreateFromPemFile(certPath, keyPath);
                return X509CertificateLoader.LoadPkcs12(
                    pem.Export(X509ContentType.Pkcs12),
                    password: null,
                    X509KeyStorageFlags.DefaultKeySet);
            }
            catch (Exception ex)
            {
                throw new InvalidOperationException(
                    $"Cannot load the agent CA from '{certPath}' and '{keyPath}': {ex.Message}",
                    ex);
            }
        }

        var directory = Path.GetDirectoryName(Path.GetFullPath(certPath));
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        using var rsa = RSA.Create(2048);
        var request = new CertificateRequest(
            "CN=" + CaCommonName,
            rsa,
            HashAlgorithmName.SHA256,
            RSASignaturePadding.Pkcs1);

        request.CertificateExtensions.Add(new X509BasicConstraintsExtension(true, false, 0, true));
        request.CertificateExtensions.Add(
            new X509KeyUsageExtension(X509KeyUsageFlags.KeyCertSign | X509KeyUsageFlags.CrlSign, true));

        using var ca = request.CreateSelfSigned(
            DateTimeOffset.UtcNow.AddDays(-1),
            DateTimeOffset.UtcNow.AddYears(10));

        File.WriteAllText(certPath, ca.ExportCertificatePem());

        var keyPem = rsa.ExportPkcs8PrivateKeyPem();
        File.WriteAllText(keyPath, keyPem);
        if (!OperatingSystem.IsWindows())
        {
            File.SetUnixFileMode(keyPath, UnixFileMode.UserRead | UnixFileMode.UserWrite);
        }

        return X509CertificateLoader.LoadPkcs12(
            ca.Export(X509ContentType.Pkcs12),
            password: null,
            X509KeyStorageFlags.DefaultKeySet);
    }
}
