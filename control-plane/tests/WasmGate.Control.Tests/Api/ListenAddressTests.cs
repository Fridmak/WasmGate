using WasmGate.Control.Api.Infrastructure.Networking;

namespace WasmGate.Control.Tests.Api;

/// <summary>Tests for listen-address parsing and its local-endpoint detection.</summary>
public sealed class ListenAddressTests
{
    [Theory]
    [InlineData("http://127.0.0.1:9096", false, "127.0.0.1", 9096)]
    [InlineData("http://localhost:9097", false, "localhost", 9097)]
    [InlineData("https://0.0.0.0:9443", true, "0.0.0.0", 9443)]
    [InlineData("http://*:8080", false, "*", 8080)]
    [InlineData("http://[::1]:9096", false, "::1", 9096)]
    public void ParsesTcpAddresses(string value, bool secure, string host, int port)
    {
        var address = ListenAddress.Parse(value);

        Assert.Equal(ListenAddressKind.Tcp, address.Kind);
        Assert.Equal(secure, address.IsSecure);
        Assert.Equal(host, address.Host);
        Assert.Equal(port, address.Port);
    }

    [Theory]
    [InlineData("http://127.0.0.1:9096", true)]
    [InlineData("http://localhost:9096", true)]
    [InlineData("http://[::1]:9096", true)]
    [InlineData("http://0.0.0.0:9096", false)]
    [InlineData("http://*:9096", false)]
    public void DetectsLocalTcpEndpoints(string value, bool isLocal)
    {
        Assert.Equal(isLocal, ListenAddress.Parse(value).IsLocal);
    }

    [Theory]
    [InlineData("127.0.0.1:9096")]
    [InlineData("http://127.0.0.1")]
    [InlineData("http://127.0.0.1:9096/path")]
    [InlineData("http://127.0.0.1:9096?x=1")]
    [InlineData("http://user@127.0.0.1:9096")]
    [InlineData("http://host.example:9096")]
    [InlineData("http://127.0.0.1:70000")]
    [InlineData("tcp://127.0.0.1:9096")]
    [InlineData("http://::1:9096")]
    public void RejectsInvalidAddresses(string value)
    {
        Assert.Throws<ArgumentException>(() => ListenAddress.Parse(value));
    }

    [Fact]
    public void ParsesUnixSocket()
    {
        var directory = Directory.CreateTempSubdirectory();
        try
        {
            var path = Path.Combine(directory.FullName, "cp.sock");
            var address = ListenAddress.Parse("unix://" + path);

            Assert.Equal(ListenAddressKind.UnixSocket, address.Kind);
            Assert.Equal(path, address.SocketPath);
            Assert.True(address.IsLocal);
        }
        finally
        {
            directory.Delete(recursive: true);
        }
    }

    [Fact]
    public void RejectsRelativeUnixSocket()
    {
        Assert.Throws<ArgumentException>(() => ListenAddress.Parse("unix://relative.sock"));
    }

    [Fact]
    public void RejectsUnixSocketInMissingDirectory()
    {
        Assert.Throws<ArgumentException>(
            () => ListenAddress.Parse("unix:///nonexistent-dir/wasmgate.sock"));
    }
}
