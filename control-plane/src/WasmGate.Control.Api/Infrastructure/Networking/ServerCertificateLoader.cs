using System.Security.Cryptography.X509Certificates;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Infrastructure.Networking;

/// <summary>
/// Loads the control-plane server certificate from the PEM files in
/// <see cref="ControlPlaneTlsOptions"/>.
/// </summary>
internal static class ServerCertificateLoader
{
    /// <summary>Loads a certificate that <c>SslStream</c> can use as a server identity.</summary>
    /// <param name="tls">TLS settings with certificate and key paths.</param>
    /// <returns>
    /// A certificate with a usable private key. It stays alive for the lifetime of
    /// the endpoint and is used on every handshake, so the caller must not dispose
    /// it after configuring Kestrel.
    /// </returns>
    /// <remarks>
    /// <c>CreateFromPemFile</c> may keep the private key in an ephemeral container
    /// that the TLS stack cannot always use. Round-tripping through PKCS#12
    /// imports the key into a durable key set, which works on every platform.
    /// </remarks>
    /// <exception cref="IOException">A PEM file cannot be read.</exception>
    /// <exception cref="System.Security.Cryptography.CryptographicException">
    /// The PEM files do not contain a matching certificate and key.
    /// </exception>
    public static X509Certificate2 Load(ControlPlaneTlsOptions tls)
    {
        using var pem = X509Certificate2.CreateFromPemFile(
            tls.CertificatePath!,
            tls.PrivateKeyPath!);

        return X509CertificateLoader.LoadPkcs12(
            pem.Export(X509ContentType.Pkcs12),
            password: null);
    }
}
