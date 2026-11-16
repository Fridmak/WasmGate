using WasmGate.Control.Infrastructure.Validation;

namespace WasmGate.Control.Tests.Infrastructure;

/// <summary>Tests for the artifact install-path guard shared by validation.</summary>
public sealed class ReleasePathTests
{
    private static readonly string Root = Path.GetTempPath();

    [Fact]
    public void AcceptsNestedRelativePath()
    {
        var target = ReleasePath.Combine(Root, "plugins/ipguard.wasm");

        Assert.Equal(Path.Combine(Root, "plugins", "ipguard.wasm"), target);
    }

    [Theory]
    [InlineData("")]
    [InlineData("   ")]
    [InlineData("../escape.wasm")]
    [InlineData("a/../../escape.wasm")]
    [InlineData("..\\escape.wasm")]
    [InlineData("plugins\\ipguard.wasm")]
    [InlineData("C:/escape.wasm")]
    [InlineData("C:\\escape.wasm")]
    [InlineData("/etc/passwd")]
    [InlineData("plugins/\0.wasm")]
    [InlineData("plugins/a\nb.wasm")]
    public void RejectsUnsafePaths(string name)
    {
        Assert.Throws<ArgumentException>(() => ReleasePath.Combine(Root, name));
    }
}
