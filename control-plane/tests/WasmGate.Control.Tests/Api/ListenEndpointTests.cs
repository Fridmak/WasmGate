using Microsoft.AspNetCore.Server.Kestrel.Core;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// The Kestrel wiring repeats the mTLS invariant so a caller that skips
/// <see cref="StartupSecurity"/> still cannot publish an anonymous network
/// agent endpoint.
/// </summary>
public sealed class ListenEndpointTests
{
    [Fact]
    public void NetworkHttpsWithoutClientCertificatesIsRejected()
    {
        var kestrel = new KestrelServerOptions();

        var exception = Assert.Throws<ArgumentException>(() =>
            ListenEndpoint.Add(
                kestrel,
                "https://0.0.0.0:9096",
                HttpProtocols.Http2,
                new ControlPlaneTlsOptions
                {
                    CertificatePath = "server.crt",
                    PrivateKeyPath = "server.key",
                    RequireClientCertificate = false,
                }));

        Assert.Contains("client certificates", exception.Message);
    }
}
