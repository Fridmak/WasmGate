using System.Net;
using System.Security.Cryptography.X509Certificates;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Microsoft.AspNetCore.Server.Kestrel.Https;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Infrastructure.Networking;

/// <summary>
/// Turns a configured listen address into a Kestrel endpoint.
/// </summary>
/// <remarks>
/// Address parsing and validation live in <see cref="ListenAddress"/>; server and
/// client certificate handling live in <see cref="ServerCertificateLoader"/> and
/// <see cref="ClientCertificateValidator"/>. This type only wires the result into
/// Kestrel.
/// </remarks>
internal static class ListenEndpoint
{
    /// <summary>Adds one Kestrel endpoint for the given address and HTTP protocol.</summary>
    /// <param name="options">Kestrel options to configure.</param>
    /// <param name="value">Listen address in a supported form.</param>
    /// <param name="protocols">HTTP protocols accepted by the endpoint.</param>
    /// <param name="tls">Optional TLS/mTLS settings; required for <c>https</c> and rejected otherwise.</param>
    /// <exception cref="ArgumentException">
    /// The address is invalid, or TLS is configured for a non-TLS address, missing
    /// for an <c>https</c> address, or cannot be loaded.
    /// </exception>
    public static void Add(
        KestrelServerOptions options,
        string value,
        HttpProtocols protocols,
        ControlPlaneTlsOptions? tls = null)
    {
        var address = ListenAddress.Parse(value);
        var hasTls = tls is { IsConfigured: true };

        if (address.Kind == ListenAddressKind.UnixSocket)
        {
            if (hasTls)
            {
                throw ListenAddress.Invalid(
                    value,
                    "TLS is not supported for unix sockets; use https:// or remove the TLS settings");
            }

            // A stale socket file is not removed automatically: deleting an
            // arbitrary path is unsafe. Use systemd RuntimeDirectory= or remove
            // the stale socket after confirming that no control plane is running.
            options.ListenUnixSocket(address.SocketPath!, listen => listen.Protocols = protocols);
            return;
        }

        if (address.IsSecure && !hasTls)
        {
            throw ListenAddress.Invalid(value, "https requires a server certificate and private key");
        }

        if (!address.IsSecure && hasTls)
        {
            throw ListenAddress.Invalid(
                value,
                "TLS settings are configured but the address uses http://; use https://");
        }

        void Configure(ListenOptions listen)
        {
            listen.Protocols = protocols;
            if (address.IsSecure)
            {
                ApplyTls(listen, tls!, value);
            }
        }

        if (address.Host == "*")
        {
            options.ListenAnyIP(address.Port, Configure);
            return;
        }

        if (string.Equals(address.Host, "localhost", StringComparison.OrdinalIgnoreCase))
        {
            options.ListenLocalhost(address.Port, Configure);
            return;
        }

        options.Listen(IPAddress.Parse(address.Host!), address.Port, Configure);
    }

    /// <summary>Configures HTTPS and optional mutual TLS on one endpoint.</summary>
    /// <param name="listen">Kestrel listen options.</param>
    /// <param name="tls">TLS settings.</param>
    /// <param name="originalValue">Original address used in error messages.</param>
    /// <exception cref="ArgumentException">The certificate, key or client CA cannot be loaded.</exception>
    private static void ApplyTls(ListenOptions listen, ControlPlaneTlsOptions tls, string originalValue)
    {
        X509Certificate2 serverCertificate;
        try
        {
            serverCertificate = ServerCertificateLoader.Load(tls);
        }
        catch (Exception ex)
        {
            throw ListenAddress.Invalid(originalValue, $"cannot load server certificate/key: {ex.Message}");
        }

        X509Certificate2Collection? clientCas = null;
        if (tls.RequireClientCertificate)
        {
            try
            {
                clientCas = ClientCertificateValidator.LoadBundle(tls.ClientCaPath);
            }
            catch (ArgumentException ex)
            {
                throw ListenAddress.Invalid(originalValue, ex.Message);
            }
        }

        listen.UseHttps(https =>
        {
            https.ServerCertificate = serverCertificate;

            // The certificate is requested, not required: gRPC services reject a
            // call without a matching agent identity, which keeps the plaintext
            // listen mode working with the same code.
            https.ClientCertificateMode = tls.RequireClientCertificate
                ? ClientCertificateMode.AllowCertificate
                : ClientCertificateMode.NoCertificate;

            if (clientCas is not null)
            {
                https.ClientCertificateValidation = (certificate, _, _) =>
                    ClientCertificateValidator.Validate(certificate, clientCas);
            }
        });
    }
}
