using Grpc.Core;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Security;
using WasmGate.Control.V1;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// Local mode is only safe on loopback and must never be combined with TLS
/// certificate settings. A network agent endpoint must always authenticate
/// callers with client certificates.
/// </summary>
public sealed class LocalModeTests
{
    [Fact]
    public void AcceptsLoopbackPlaintext()
    {
        StartupSecurity.Validate(new ControlPlaneOptions
        {
            Local = true,
            HttpListen = "http://127.0.0.1:9097",
            GrpcListen = "http://127.0.0.1:9096",
        });
    }

    [Fact]
    public void RejectsNonLoopbackAdmin()
    {
        var exception = Assert.Throws<InvalidOperationException>(() =>
            StartupSecurity.Validate(new ControlPlaneOptions
            {
                Local = true,
                HttpListen = "http://0.0.0.0:9097",
                GrpcListen = "http://127.0.0.1:9096",
            }));
        Assert.Contains("admin endpoint", exception.Message);
    }

    [Fact]
    public void RejectsNonLoopbackGrpc()
    {
        var exception = Assert.Throws<InvalidOperationException>(() =>
            StartupSecurity.Validate(new ControlPlaneOptions
            {
                Local = true,
                HttpListen = "http://127.0.0.1:9097",
                GrpcListen = "http://0.0.0.0:9096",
            }));
        Assert.Contains("agent endpoint", exception.Message);
    }

    [Fact]
    public void RejectsTlsSettings()
    {
        var exception = Assert.Throws<InvalidOperationException>(() =>
            StartupSecurity.Validate(new ControlPlaneOptions
            {
                Local = true,
                HttpListen = "http://127.0.0.1:9097",
                GrpcListen = "http://127.0.0.1:9096",
                GrpcTls = new ControlPlaneTlsOptions
                {
                    CertificatePath = "server.crt",
                    PrivateKeyPath = "server.key",
                },
            }));
        Assert.Contains("cannot be combined with TLS", exception.Message);
    }

    [Fact]
    public void RejectsPartialTlsSettings()
    {
        var exception = Assert.Throws<InvalidOperationException>(() =>
            StartupSecurity.Validate(new ControlPlaneOptions
            {
                Local = true,
                HttpListen = "http://127.0.0.1:9097",
                GrpcListen = "http://127.0.0.1:9096",
                GrpcTls = new ControlPlaneTlsOptions
                {
                    CertificatePath = "server.crt",
                },
            }));
        Assert.Contains("cannot be combined with TLS", exception.Message);
    }

    [Fact]
    public void RejectsSecureListenerWithoutTls()
    {
        var exception = Assert.Throws<InvalidOperationException>(() =>
            StartupSecurity.Validate(new ControlPlaneOptions
            {
                Local = true,
                HttpListen = "http://127.0.0.1:9097",
                GrpcListen = "https://127.0.0.1:9096",
            }));
        Assert.Contains("cannot be combined with TLS", exception.Message);
    }

    [Fact]
    public void NetworkHttpsRequiresClientCertificates()
    {
        var exception = Assert.Throws<InvalidOperationException>(() =>
            StartupSecurity.Validate(new ControlPlaneOptions
            {
                HttpListen = "http://0.0.0.0:9097",
                GrpcListen = "https://0.0.0.0:9096",
                AdminToken = "token",
                GrpcTls = new ControlPlaneTlsOptions
                {
                    CertificatePath = "server.crt",
                    PrivateKeyPath = "server.key",
                    RequireClientCertificate = false,
                },
            }));
        Assert.Contains("client certificates", exception.Message);
    }

    [Fact]
    public void NetworkHttpsAcceptsRequiredClientCertificates()
    {
        StartupSecurity.Validate(new ControlPlaneOptions
        {
            HttpListen = "http://0.0.0.0:9097",
            GrpcListen = "https://0.0.0.0:9096",
            AdminToken = "token",
            GrpcTls = new ControlPlaneTlsOptions
            {
                CertificatePath = "server.crt",
                PrivateKeyPath = "server.key",
                RequireClientCertificate = true,
            },
        });
    }

    [Fact]
    public void LoopbackHttpsMayDisableClientCertificates()
    {
        // Loopback is a single-user trust boundary, so the development
        // convenience of omitting client certificates is still allowed there.
        StartupSecurity.Validate(new ControlPlaneOptions
        {
            HttpListen = "http://127.0.0.1:9097",
            GrpcListen = "https://127.0.0.1:9096",
            GrpcTls = new ControlPlaneTlsOptions
            {
                CertificatePath = "server.crt",
                PrivateKeyPath = "server.key",
                RequireClientCertificate = false,
            },
        });
    }

    [Fact]
    public void PlaintextModeDoesNotCreateCa()
    {
        var directory = Directory.CreateTempSubdirectory("wasmgate-local-ca-").FullName;
        try
        {
            var options = new ControlPlaneOptions
            {
                Local = true,
                AgentCaCertPath = Path.Combine(directory, "agents-ca.crt"),
                AgentCaKeyPath = Path.Combine(directory, "agents-ca.key"),
            };

            _ = new PkiCertificateAuthority(options);

            Assert.False(File.Exists(options.AgentCaCertPath));
            Assert.False(File.Exists(options.AgentCaKeyPath));
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public void TlsWithClientCertificatesCreatesCa()
    {
        var directory = Directory.CreateTempSubdirectory("wasmgate-tls-ca-").FullName;
        try
        {
            var options = new ControlPlaneOptions
            {
                AgentCaCertPath = Path.Combine(directory, "agents-ca.crt"),
                AgentCaKeyPath = Path.Combine(directory, "agents-ca.key"),
                GrpcTls = new ControlPlaneTlsOptions
                {
                    CertificatePath = "server.crt",
                    PrivateKeyPath = "server.key",
                    RequireClientCertificate = true,
                },
            };

            _ = new PkiCertificateAuthority(options);

            Assert.True(File.Exists(options.AgentCaCertPath));
            Assert.True(File.Exists(options.AgentCaKeyPath));
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public async Task EnrollmentIsDisabledInLocalMode()
    {
        var service = new AgentEnrollmentService(
            provisioning: null!,
            certificates: null!,
            new ControlPlaneOptions { Local = true },
            NullLogger<AgentEnrollmentService>.Instance);

        var exception = await Assert.ThrowsAsync<RpcException>(
            () => service.Enroll(new EnrollRequest(), null!));
        Assert.Equal(StatusCode.Unimplemented, exception.StatusCode);
    }

    [Fact]
    public async Task RenewalIsDisabledInLocalMode()
    {
        var service = new AgentEnrollmentService(
            provisioning: null!,
            certificates: null!,
            new ControlPlaneOptions { Local = true },
            NullLogger<AgentEnrollmentService>.Instance);

        var exception = await Assert.ThrowsAsync<RpcException>(
            () => service.Renew(new RenewRequest(), null!));
        Assert.Equal(StatusCode.Unimplemented, exception.StatusCode);
    }
}
