using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using WasmGate.Control.Api.Infrastructure.Networking;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// The TLS layer validates the agent certificate chain and client-auth EKU
/// before the gRPC service sees the call. These tests pin that contract.
/// </summary>
public sealed class ClientCertificateValidatorTests
{
    private const string ClientAuthOid = "1.3.6.1.5.5.7.3.2";
    private const string ServerAuthOid = "1.3.6.1.5.5.7.3.1";

    [Fact]
    public void AcceptsClientCertificateSignedByTrustedCa()
    {
        using var pki = new TestPki();

        using var client = pki.IssueClientCertificate(
            TimeSpan.FromHours(-1),
            TimeSpan.FromHours(1),
            ClientAuthOid);

        Assert.True(ClientCertificateValidator.Validate(client, pki.Trusted));
    }

    [Fact]
    public void RejectsCertificateSignedByAnotherCa()
    {
        using var trusted = new TestPki();
        using var other = new TestPki();

        using var client = other.IssueClientCertificate(
            TimeSpan.FromHours(-1),
            TimeSpan.FromHours(1),
            ClientAuthOid);

        Assert.False(ClientCertificateValidator.Validate(client, trusted.Trusted));
    }

    [Fact]
    public void RejectsExpiredCertificate()
    {
        using var pki = new TestPki();

        using var client = pki.IssueClientCertificate(
            TimeSpan.FromDays(-2),
            TimeSpan.FromDays(-1),
            ClientAuthOid);

        Assert.False(ClientCertificateValidator.Validate(client, pki.Trusted));
    }

    [Fact]
    public void RejectsServerOnlyExtendedKeyUsage()
    {
        using var pki = new TestPki();

        using var client = pki.IssueClientCertificate(
            TimeSpan.FromHours(-1),
            TimeSpan.FromHours(1),
            ServerAuthOid);

        Assert.False(ClientCertificateValidator.Validate(client, pki.Trusted));
    }

    /// <summary>A small CA that issues client certificates for the tests.</summary>
    private sealed class TestPki : IDisposable
    {
        private readonly RSA _caKey = RSA.Create(2048);
        private readonly X509Certificate2 _ca;

        public TestPki()
        {
            var request = new CertificateRequest(
                "CN=WasmGate Test CA",
                _caKey,
                HashAlgorithmName.SHA256,
                RSASignaturePadding.Pkcs1);
            request.CertificateExtensions.Add(
                new X509BasicConstraintsExtension(
                    certificateAuthority: true,
                    hasPathLengthConstraint: false,
                    pathLengthConstraint: 0,
                    critical: true));
            request.CertificateExtensions.Add(
                new X509KeyUsageExtension(
                    X509KeyUsageFlags.KeyCertSign | X509KeyUsageFlags.CrlSign,
                    critical: true));

            _ca = request.CreateSelfSigned(
                DateTimeOffset.UtcNow.AddDays(-30),
                DateTimeOffset.UtcNow.AddDays(30));

            Trusted = new X509Certificate2Collection();
            Trusted.Add(
                X509CertificateLoader.LoadCertificate(
                    _ca.Export(X509ContentType.Cert)));
        }

        public X509Certificate2Collection Trusted { get; }

        public X509Certificate2 IssueClientCertificate(
            TimeSpan notBeforeOffset,
            TimeSpan notAfterOffset,
            string enhancedKeyUsageOid)
        {
            using var key = RSA.Create(2048);
            var request = new CertificateRequest(
                "CN=wasmgate-agent:" + Guid.NewGuid().ToString("D"),
                key,
                HashAlgorithmName.SHA256,
                RSASignaturePadding.Pkcs1);
            request.CertificateExtensions.Add(
                new X509BasicConstraintsExtension(
                    certificateAuthority: false,
                    hasPathLengthConstraint: false,
                    pathLengthConstraint: 0,
                    critical: false));
            request.CertificateExtensions.Add(
                new X509KeyUsageExtension(
                    X509KeyUsageFlags.DigitalSignature,
                    critical: false));
            request.CertificateExtensions.Add(
                new X509EnhancedKeyUsageExtension(
                    new OidCollection { new Oid(enhancedKeyUsageOid) },
                    critical: false));

            using var issued = request.Create(
                _ca,
                DateTimeOffset.UtcNow + notBeforeOffset,
                DateTimeOffset.UtcNow + notAfterOffset,
                RandomNumberGenerator.GetBytes(16));

            return X509CertificateLoader.LoadCertificate(
                issued.Export(X509ContentType.Cert));
        }

        public void Dispose()
        {
            foreach (var certificate in Trusted)
            {
                certificate.Dispose();
            }

            _ca.Dispose();
            _caKey.Dispose();
        }
    }
}
