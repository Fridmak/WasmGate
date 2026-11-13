using System.Globalization;
using System.Net;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Microsoft.AspNetCore.Server.Kestrel.Https;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Infrastructure;

/// <summary>
/// Parses a listen address into a Kestrel endpoint.
/// </summary>
/// <remarks>
/// Supported forms:
/// <list type="bullet">
///   <item><c>http://&lt;ip|localhost|*&gt;:&lt;port&gt;</c> — plaintext HTTP endpoint;</item>
///   <item><c>https://&lt;ip|localhost|*&gt;:&lt;port&gt;</c> — TLS endpoint;</item>
///   <item><c>unix:///absolute/path/to.sock</c> — Unix domain socket.</item>
/// </list>
/// TLS settings are supplied separately. <c>https</c> requires a configured
/// server certificate; when mutual TLS is enabled, client certificates are
/// validated against the configured CA bundle. A port is always required; the
/// path, query, fragment and userinfo components of an HTTP URI are rejected.
/// <c>*</c> binds all interfaces, <c>localhost</c> binds both loopback families,
/// and an explicit IP address binds that address only.
/// </remarks>
internal static class ListenEndpoint
{
    private const string UnixPrefix = "unix://";
    private const string SchemeSeparator = "://";

    /// <summary>Adds one Kestrel endpoint for the given address and HTTP protocol.</summary>
    /// <param name="options">Kestrel options to configure.</param>
    /// <param name="value">Listen address in a supported form.</param>
    /// <param name="protocols">HTTP protocols accepted by the endpoint.</param>
    /// <param name="tls">Optional TLS/mTLS settings for an HTTPS endpoint.</param>
    /// <exception cref="ArgumentException">
    /// The address is empty, has an unsupported scheme, omits the port, contains
    /// an unsupported URI component, or has an invalid host/port/socket path or
    /// TLS configuration.
    /// </exception>
    public static void Add(
        KestrelServerOptions options,
        string value,
        HttpProtocols protocols,
        ControlPlaneTlsOptions? tls = null)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(value);
        value = value.Trim();

        if (value.StartsWith(UnixPrefix, StringComparison.OrdinalIgnoreCase))
        {
            if (tls is { IsConfigured: true })
            {
                throw Invalid(value, "TLS is not supported for unix sockets; use https:// or remove the TLS settings");
            }

            AddUnixSocket(options, value[UnixPrefix.Length..], protocols, value);
            return;
        }

        var schemeEnd = value.IndexOf(SchemeSeparator, StringComparison.Ordinal);
        if (schemeEnd <= 0)
        {
            throw Invalid(value, "expected http://, https:// or unix:///absolute/path.sock");
        }

        var scheme = value[..schemeEnd];
        var useTls = scheme.Equals("https", StringComparison.OrdinalIgnoreCase);
        if (!useTls && !scheme.Equals("http", StringComparison.OrdinalIgnoreCase))
        {
            var hint = scheme.Equals("tcp", StringComparison.OrdinalIgnoreCase)
                ? "use http:// or https:// for the gRPC/admin HTTP endpoint or unix:// for a socket"
                : $"unsupported scheme '{scheme}'; use http://, https:// or unix://";
            throw Invalid(value, hint);
        }

        if (useTls && (tls is null || !tls.IsConfigured))
        {
            throw Invalid(value, "https requires a server certificate and private key");
        }
        if (!useTls && tls is { IsConfigured: true })
        {
            throw Invalid(value, "TLS settings are configured but the address uses http://; use https://");
        }

        var authorityStart = schemeEnd + SchemeSeparator.Length;
        var authorityEnd = value.IndexOfAny(['/', '?', '#'], authorityStart);
        var authority = authorityEnd < 0 ? value[authorityStart..] : value[authorityStart..authorityEnd];
        var tail = authorityEnd < 0 ? string.Empty : value[authorityEnd..];

        if (authority.Length == 0)
        {
            throw Invalid(value, "host and port are required");
        }
        if (authority.Contains('@'))
        {
            throw Invalid(value, "userinfo is not allowed");
        }
        if (tail.Length > 0 && tail != "/")
        {
            throw Invalid(value, "path, query and fragment are not allowed");
        }

        var (host, portText) = SplitAuthority(authority, value);
        if (!int.TryParse(portText, NumberStyles.None, CultureInfo.InvariantCulture, out var port) ||
            port is < 0 or > 65535)
        {
            throw Invalid(value, "port must be a number between 0 and 65535");
        }

        Action<ListenOptions> configure = listen =>
        {
            listen.Protocols = protocols;
            if (useTls)
            {
                ConfigureTls(listen, tls!, value);
            }
        };

        if (host == "*")
        {
            options.ListenAnyIP(port, configure);
            return;
        }

        if (host.Equals("localhost", StringComparison.OrdinalIgnoreCase))
        {
            options.ListenLocalhost(port, configure);
            return;
        }

        if (!IPAddress.TryParse(host, out var address))
        {
            throw Invalid(value, "host must be an IP address, 'localhost' or '*'");
        }

        options.Listen(address, port, configure);
    }

    /// <summary>Configures HTTPS and optional mutual TLS on one Kestrel endpoint.</summary>
    /// <param name="listen">Kestrel listen options.</param>
    /// <param name="tls">TLS settings.</param>
    /// <param name="originalValue">Original address used in error messages.</param>
    /// <exception cref="ArgumentException">The certificate, key or client CA cannot be loaded.</exception>
    private static void ConfigureTls(
        ListenOptions listen,
        ControlPlaneTlsOptions tls,
        string originalValue)
    {
        X509Certificate2 serverCertificate;
        try
        {
            // CreateFromPemFile returns a certificate whose private key may be
            // ephemeral. Re-importing through PKCS#12 gives SslStream a key it
            // can use for the server certificate on all platforms.
            using var pem = X509Certificate2.CreateFromPemFile(
                tls.CertificatePath!,
                tls.PrivateKeyPath!);
            var pfx = pem.Export(X509ContentType.Pkcs12);
            serverCertificate = X509CertificateLoader.LoadPkcs12(
                pfx,
                password: null,
                X509KeyStorageFlags.DefaultKeySet);
        }
        catch (Exception ex)
        {
            throw Invalid(originalValue, $"cannot load server certificate/key: {ex.Message}");
        }

        X509Certificate2Collection? clientCas = null;
        if (tls.RequireClientCertificate)
        {
            if (string.IsNullOrWhiteSpace(tls.ClientCaPath))
            {
                throw Invalid(originalValue, "client certificate authentication requires a client CA bundle");
            }

            try
            {
                clientCas = new X509Certificate2Collection();
                clientCas.ImportFromPem(File.ReadAllText(tls.ClientCaPath));
            }
            catch (Exception ex)
            {
                throw Invalid(originalValue, $"cannot load client CA bundle: {ex.Message}");
            }

            if (clientCas.Count == 0)
            {
                throw Invalid(originalValue, "client CA bundle contains no certificates");
            }
        }

        listen.UseHttps(https =>
        {
            https.ServerCertificate = serverCertificate;
            https.ClientCertificateMode = tls.RequireClientCertificate
                ? ClientCertificateMode.AllowCertificate
                : ClientCertificateMode.NoCertificate;

            if (clientCas is not null)
            {
                https.ClientCertificateValidation = (certificate, _, _) =>
                    ValidateClientCertificate(certificate, clientCas);
            }
        });
    }

    /// <summary>Validates an agent client certificate against the configured CA bundle.</summary>
    /// <param name="certificate">Client certificate presented by the agent.</param>
    /// <param name="trustedCas">CA certificates trusted for agent identities.</param>
    /// <returns>True when the chain builds to one of the trusted CAs.</returns>
    private static bool ValidateClientCertificate(
        X509Certificate2? certificate,
        X509Certificate2Collection trustedCas)
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

        // If an EKU is present, require the client-auth purpose. Certificates
        // without an EKU are accepted because some private CAs omit it.
        var eku = certificate.Extensions
            .OfType<X509EnhancedKeyUsageExtension>()
            .FirstOrDefault();

        return eku is null ||
               eku.EnhancedKeyUsages
                   .Cast<Oid>()
                   .Any(oid => oid.Value == "1.3.6.1.5.5.7.3.2");
    }

    /// <summary>Splits an HTTP authority into host and port.</summary>
    /// <param name="authority">Authority component, for example localhost:9096 or [::1]:9096.</param>
    /// <param name="value">Original address used in error messages.</param>
    /// <returns>Host without IPv6 brackets and the explicit port text.</returns>
    /// <exception cref="ArgumentException">The authority has no port or invalid IPv6 brackets.</exception>
    private static (string Host, string PortText) SplitAuthority(string authority, string value)
    {
        if (authority[0] == '[')
        {
            var close = authority.IndexOf(']');
            if (close <= 1)
            {
                throw Invalid(value, "invalid IPv6 host");
            }

            var host = authority[1..close];
            var rest = authority[(close + 1)..];
            if (!rest.StartsWith(':'))
            {
                throw Invalid(value, "port is required");
            }

            return (host, rest[1..]);
        }

        var colon = authority.LastIndexOf(':');
        if (colon <= 0)
        {
            throw Invalid(value, "port is required");
        }

        var unbracketedHost = authority[..colon];
        if (unbracketedHost.Contains(':'))
        {
            throw Invalid(value, "IPv6 hosts must be enclosed in brackets");
        }

        return (unbracketedHost, authority[(colon + 1)..]);
    }

    /// <summary>Configures a Kestrel Unix-domain-socket endpoint.</summary>
    /// <param name="options">Kestrel options to configure.</param>
    /// <param name="path">Absolute socket path.</param>
    /// <param name="protocols">HTTP protocols accepted by the endpoint.</param>
    /// <param name="originalValue">Original address used in error messages.</param>
    /// <exception cref="ArgumentException">The path is empty, relative, too long, or its directory is missing.</exception>
    /// <remarks>
    /// A stale socket file is not removed automatically. Use systemd
    /// RuntimeDirectory= or remove the stale socket after confirming that no
    /// control plane is running.
    /// </remarks>
    private static void AddUnixSocket(
        KestrelServerOptions options,
        string path,
        HttpProtocols protocols,
        string originalValue)
    {
        if (path.Length == 0)
        {
            throw Invalid(originalValue, "unix socket path is required");
        }
        if (!Path.IsPathFullyQualified(path))
        {
            throw Invalid(originalValue, "unix socket path must be absolute");
        }
        if (Directory.Exists(path))
        {
            throw Invalid(originalValue, "unix socket path points to a directory");
        }

        var directory = Path.GetDirectoryName(path);
        if (string.IsNullOrEmpty(directory) || !Directory.Exists(directory))
        {
            throw Invalid(
                originalValue,
                $"unix socket directory '{directory}' does not exist; create it before starting the control plane");
        }

        if (!OperatingSystem.IsWindows())
        {
            var maxBytes = OperatingSystem.IsMacOS() ? 103 : 107;
            if (System.Text.Encoding.UTF8.GetByteCount(path) > maxBytes)
            {
                throw Invalid(originalValue, $"unix socket path is too long (maximum {maxBytes} bytes)");
            }
        }

        // A stale socket file is not removed automatically: deleting an
        // arbitrary path is unsafe. Use systemd RuntimeDirectory= or remove the
        // stale socket explicitly after confirming that no control plane is
        // running.
        options.ListenUnixSocket(path, listen => listen.Protocols = protocols);
    }

    /// <summary>Creates a configuration error with a consistent message.</summary>
    /// <param name="value">Invalid listen address.</param>
    /// <param name="reason">Human-readable reason.</param>
    /// <returns>An exception suitable for startup failure.</returns>
    private static ArgumentException Invalid(string value, string reason) =>
        new($"Invalid listen address '{value}': {reason}.");
}
