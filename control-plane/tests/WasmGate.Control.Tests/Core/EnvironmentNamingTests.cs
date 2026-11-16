using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Releases;

namespace WasmGate.Control.Tests.Core;

/// <summary>Tests for environment-name normalisation and validation.</summary>
public sealed class EnvironmentNamingTests
{
    [Theory]
    [InlineData("prod", "prod")]
    [InlineData("  Prod  ", "prod")]
    [InlineData("Stage.EU_1", "stage.eu_1")]
    public void NormalizeTrimsAndLowerCases(string input, string expected)
    {
        Assert.Equal(expected, EnvironmentNaming.Normalize(input));
    }

    [Theory]
    [InlineData("prod")]
    [InlineData("stage-1")]
    [InlineData("a.b_c")]
    [InlineData("0")]
    public void AcceptsSafeNames(string name)
    {
        Assert.True(EnvironmentNaming.IsValid(name));
    }

    [Theory]
    [InlineData("")]
    [InlineData(" ")]
    [InlineData("-edge")]
    [InlineData("edge-")]
    [InlineData("a b")]
    [InlineData("prod/eu")]
    [InlineData("среда")]
    public void RejectsUnsafeNames(string name)
    {
        Assert.False(EnvironmentNaming.IsValid(name));
    }

    [Fact]
    public void RejectsNameLongerThanLimit()
    {
        Assert.True(EnvironmentNaming.IsValid(new string('a', EnvironmentNaming.MaxLength)));
        Assert.False(EnvironmentNaming.IsValid(new string('a', EnvironmentNaming.MaxLength + 1)));
    }
}
