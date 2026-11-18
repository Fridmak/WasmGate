using Grpc.Core;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.V1;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// Local mode is only safe on loopback and must never be combined with TLS
/// certificate settings.
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
}
